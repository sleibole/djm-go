package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

const (
	// smtpSendTimeout bounds one delivery attempt, including the dial.
	smtpSendTimeout = 5 * time.Second
	// smtpRetryAfter is the pause before the single retry.
	smtpRetryAfter = 500 * time.Millisecond
	smtpAttempts   = 2
)

type mailer interface {
	Send(to, subject, body string) error
}

// consoleMailer logs emails instead of sending them, for development.
type consoleMailer struct {
	logger *slog.Logger
}

func (m consoleMailer) Send(to, subject, body string) error {
	m.logger.Info("email not sent (no SMTP_HOST configured)", "to", to, "subject", subject, "body", body)
	return nil
}

// smtpMailer sends plain-text email through a transactional relay. It upgrades
// to TLS with STARTTLS when the server offers it, which PlainAuth requires.
// Each attempt has a deadline. A transient failure is retried once.
type smtpMailer struct {
	addr        string // host:port
	username    string
	password    string
	from        *mail.Address
	timeout     time.Duration
	retryAfter  time.Duration
	requireTLS  bool // prod: do not send login links on a connection that never became TLS
	implicitTLS bool // port 465: TLS from the first byte, not STARTTLS
}

// errSMTPCleartext is returned when production mail would otherwise go out unencrypted.
var errSMTPCleartext = errors.New("smtp: refusing to send without TLS")

func (m smtpMailer) Send(to, subject, body string) error {
	host, _, err := net.SplitHostPort(m.addr)
	if err != nil {
		return err
	}

	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", m.from.String())
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	fmt.Fprintf(&msg, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	msg.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	raw := []byte(msg.String())

	var last error
	for attempt := 1; attempt <= smtpAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(m.retryAfter)
		}
		last = m.sendOnce(host, to, raw)
		if last == nil || !transientSMTPError(last) {
			return last
		}
	}
	return last
}

// sendOnce dials, speaks SMTP, and returns. The deadline covers the dial and
// every later read and write on that connection.
func (m smtpMailer) sendOnce(host, to string, msg []byte) error {
	deadline := time.Now().Add(m.timeout)
	dialer := &net.Dialer{Deadline: deadline}
	var conn net.Conn
	var err error
	if m.implicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", m.addr, &tls.Config{ServerName: host})
	} else {
		conn, err = dialer.Dial("tcp", m.addr)
	}
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return err
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	// Same conversation as smtp.SendMail: greet, STARTTLS when offered, then AUTH.
	if err := c.Hello("localhost"); err != nil {
		return err
	}
	tlsOn := m.implicitTLS
	if !m.implicitTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return err
			}
			tlsOn = true
		}
	}
	if m.requireTLS && !tlsOn {
		return errSMTPCleartext
	}
	var auth smtp.Auth
	if m.username != "" {
		auth = smtp.PlainAuth("", m.username, m.password, host)
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: server doesn't support AUTH")
		}
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(m.from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// transientSMTPError reports whether a second attempt is worthwhile.
// A 5xx reply means the server rejected the message. Anything else — a 4xx
// reply, a timeout, or a broken connection — is tried once more.
func transientSMTPError(err error) bool {
	if errors.Is(err, errSMTPCleartext) {
		return false
	}
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return reply.Code >= 400 && reply.Code < 500
	}
	return true
}

func newMailer(cfg config, logger *slog.Logger) (mailer, error) {
	if cfg.SMTPHost == "" {
		return consoleMailer{logger: logger}, nil
	}
	from, err := mail.ParseAddress(cfg.MailFrom)
	if err != nil {
		return nil, fmt.Errorf("MAIL_FROM: %w", err)
	}
	return smtpMailer{
		addr:        net.JoinHostPort(cfg.SMTPHost, cfg.SMTPPort),
		username:    cfg.SMTPUser,
		password:    cfg.SMTPPass,
		from:        from,
		timeout:     smtpSendTimeout,
		retryAfter:  smtpRetryAfter,
		requireTLS:  cfg.Env == "prod",
		implicitTLS: cfg.SMTPPort == "465",
	}, nil
}
