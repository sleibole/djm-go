package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"
)

//go:embed templates static
var embedded embed.FS

func init() {
	// Go's built-in MIME table doesn't know this extension; browsers expect this type.
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

type app struct {
	cfg    config
	db     *gorm.DB
	views  *views
	static assets
	logger *slog.Logger
	mailer mailer
	now    func() time.Time // replaced in tests to move the clock

	loginLimiter     *rateLimiter
	linkIPLimiter    *rateLimiter
	linkEmailLimiter *rateLimiter
	uploadLimiter    *rateLimiter
	audienceLimiter  *rateLimiter // song requests per IP and show
	turnstile        *turnstileVerifier

	blobs     blobStore
	cache     *catalogCache
	buildWake chan struct{}
}

func main() {
	if err := loadDotEnv(".env"); err != nil {
		slog.Error("load .env", "err", err)
		os.Exit(1)
	}
	cfg := loadConfig()
	logger := newLogger(cfg.Env)

	if err := run(cfg, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newApp(cfg config, db *gorm.DB, logger *slog.Logger, m mailer) (*app, error) {
	static, err := hashAssets(embedded)
	if err != nil {
		return nil, err
	}
	v, err := loadViews(embedded, static)
	if err != nil {
		return nil, err
	}
	cfg.applyTurnstileDevDefaults()
	a := &app{
		cfg: cfg, db: db, views: v, static: static, logger: logger, mailer: m, now: time.Now,
		turnstile: newTurnstileVerifier(cfg.TurnstileSecretKey),
		blobs:     dirBlobStore{dir: cfg.BlobDir},
		buildWake: make(chan struct{}, 1),
	}
	clock := func() time.Time { return a.now() }
	// The email limits are the Rails ones: 5 per address and 10 per IP each hour.
	a.loginLimiter = newRateLimiter(10, time.Minute, clock)
	a.linkIPLimiter = newRateLimiter(10, time.Hour, clock)
	a.linkEmailLimiter = newRateLimiter(5, time.Hour, clock)
	a.uploadLimiter = newRateLimiter(5, time.Hour, clock)
	a.audienceLimiter = newRateLimiter(audienceRequestLimit, audienceRequestWindow, clock)
	a.cache, err = newCatalogCache(cfg.CatalogCacheDir, cfg.CatalogCacheMax, a.blobs, logger, clock)
	if err != nil {
		return nil, fmt.Errorf("catalog cache: %w", err)
	}
	a.refreshPins()
	return a, nil
}

func run(cfg config, logger *slog.Logger) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	db, err := openDB(cfg.DBPath)
	if err != nil {
		return err
	}

	m, err := newMailer(cfg, logger)
	if err != nil {
		return err
	}
	if cfg.Env == "prod" && cfg.SMTPHost == "" {
		logger.Warn("SMTP_HOST is not set; magic-link emails will only be logged")
	}

	a, err := newApp(cfg, db, logger, m)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var workers sync.WaitGroup
	workers.Go(func() { a.cleanup(ctx) })
	workers.Go(func() { a.runBuilder(ctx) })
	defer func() {
		stop()
		workers.Wait()
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "env", cfg.Env, "db", cfg.DBPath)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
	}
	return nil
}

func newLogger(env string) *slog.Logger {
	if env == "prod" {
		return slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", a.static.handler(embedded))
	mux.HandleFunc("GET /favicon.ico", a.handleFavicon)
	mux.HandleFunc("GET /healthz", a.handleHealthz)
	mux.HandleFunc("GET /{$}", a.handleHome)

	mux.HandleFunc("GET /signin", a.handleSigninForm)
	mux.HandleFunc("GET /signup", a.handleSignupForm)
	mux.HandleFunc("POST /signin", a.handlePasswordLogin)
	mux.HandleFunc("POST /signin/link", a.handleLinkRequest)
	mux.HandleFunc("GET /auth/{token}", a.handleLinkConfirm)
	mux.HandleFunc("POST /auth/{token}", a.handleLinkRedeem)
	mux.HandleFunc("POST /logout", a.handleLogout)

	mux.HandleFunc("GET /account", a.requireUser(a.handleAccount))
	mux.HandleFunc("POST /account/password", a.requireUser(a.handleAccountPassword))
	mux.HandleFunc("POST /account/password/delete", a.requireUser(a.handleAccountPasswordRemove))
	mux.HandleFunc("POST /account/profile", a.requireUser(a.handleAccountProfile))
	mux.HandleFunc("POST /account/defaults", a.requireUser(a.handleAccountDefaults))
	mux.HandleFunc("POST /account/host", a.requireUser(a.handleAccountHost))
	mux.HandleFunc("POST /account/delete", a.requireUser(a.handleAccountDelete))

	mux.HandleFunc("GET /catalogs", a.requireHost(a.handleCatalogs))
	mux.HandleFunc("GET /catalogs/new", a.requireHost(a.handleNewCatalog))
	mux.HandleFunc("POST /catalogs", a.requireHost(a.handleCreateCatalog))
	mux.HandleFunc("GET /catalogs/{id}", a.requireHost(a.handleCatalog))
	mux.HandleFunc("POST /catalogs/{id}", a.requireHost(a.handleCatalogSettings))
	mux.HandleFunc("POST /catalogs/{id}/upload", a.requireHost(a.handleCatalogUpload))
	mux.HandleFunc("GET /catalogs/{id}/status", a.requireHost(a.handleCatalogStatus))
	mux.HandleFunc("GET /catalogs/{id}/search", a.requireHost(a.handleCatalogSearch))
	mux.HandleFunc("POST /catalogs/{id}/delete", a.requireHost(a.handleCatalogDelete))

	mux.HandleFunc("GET /shows", a.requireHost(a.handleShows))
	mux.HandleFunc("GET /shows/new", a.requireHost(a.handleNewShow))
	mux.HandleFunc("POST /shows", a.requireHost(a.handleCreateShow))
	mux.HandleFunc("GET /shows/{id}", a.requireHost(a.handleShow))
	mux.HandleFunc("POST /shows/{id}", a.requireHost(a.handleShowSettings))
	mux.HandleFunc("POST /shows/{id}/start", a.requireHost(a.handleStartShow))
	mux.HandleFunc("POST /shows/{id}/end", a.requireHost(a.handleEndShow))
	mux.HandleFunc("GET /shows/{id}/live", a.requireHost(a.handleShowLive))
	mux.HandleFunc("GET /shows/{id}/search", a.requireHost(a.handleShowSearch))
	mux.HandleFunc("GET /shows/{id}/participants", a.requireHost(a.handleParticipantSuggestions))
	mux.HandleFunc("POST /shows/{id}/requests", a.requireHost(a.handleAddRequest))
	mux.HandleFunc("POST /requests/{rid}/{action}", a.requireHost(a.handleRequestAction))

	mux.HandleFunc("GET /dj/{handle}", a.handleDJProfile)
	mux.HandleFunc("GET /dj/{handle}/shows/{slug}", a.handleAudienceShow)
	mux.HandleFunc("GET /dj/{handle}/shows/{slug}/search", a.handleAudienceSearch)
	mux.HandleFunc("POST /dj/{handle}/shows/{slug}/requests", a.handleAudienceRequest)
	mux.HandleFunc("POST /dj/{handle}/shows/{slug}/requests/{rid}/cancel", a.handleAudienceCancel)
	mux.HandleFunc("GET /dj/{handle}/shows/{slug}/queue", a.handleAudienceQueue)
	mux.HandleFunc("GET /dj/{handle}/shows/{slug}/display", a.handleDisplay)

	mux.HandleFunc("GET /docs/csv-format", a.handleCSVFormatDoc)

	mux.HandleFunc("/", a.notFound)

	csrf := http.NewCrossOriginProtection()
	return a.recoverPanic(a.logRequests(securityHeaders(csrf.Handler(a.loadUser(mux)))))
}
