package main

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
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

func newApp(cfg config, db *gorm.DB, logger *slog.Logger) (*app, error) {
	static, err := hashAssets(embedded)
	if err != nil {
		return nil, err
	}
	v, err := loadViews(embedded, static)
	if err != nil {
		return nil, err
	}
	return &app{cfg: cfg, db: db, views: v, static: static, logger: logger}, nil
}

func run(cfg config, logger *slog.Logger) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	db, err := openDB(cfg.DBPath)
	if err != nil {
		return err
	}

	a, err := newApp(cfg, db, logger)
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
	defer stop()

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
	mux.HandleFunc("/", a.notFound)

	csrf := http.NewCrossOriginProtection()
	return a.recoverPanic(a.logRequests(securityHeaders(csrf.Handler(mux))))
}
