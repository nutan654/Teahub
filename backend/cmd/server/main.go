// Command server runs the TeaHub backend: repo import + AI Q&A over a codebase.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"teahub/backend/internal/api"
	"teahub/backend/internal/config"
	"teahub/backend/internal/dbstore"
	"teahub/backend/internal/httpmw"
	"teahub/backend/internal/memstore"
	"teahub/backend/internal/store"
)

// version is overridden at build time via -ldflags "-X main.version=...";
// see Dockerfile. Left as "dev" for local `go run`/`go build` so it's
// obvious in logs/health output when a binary wasn't built through the
// pinned Docker build.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := httpmw.NewLogger(cfg.Env)

	if cfg.AnthropicAPIKey == "" {
		logger.Warn("anthropic_api_key_unset", "detail", "/api/import will still work; /api/ask, /api/repos/{id}/diagram, and /api/repos/{id}/search will fail until ANTHROPIC_API_KEY is set")
	}
	if cfg.GitHubClientID == "" {
		logger.Warn("github_oauth_unset", "detail", "sign-in is disabled until GITHUB_CLIENT_ID/GITHUB_CLIENT_SECRET are set")
	}
	if len(cfg.JWTSigningKey) == 0 {
		logger.Warn("jwt_signing_key_generated", "detail", "JWT_SIGNING_KEY was not set; a random key was generated for this process only, so sessions will not survive a restart")
		key, genErr := randomKey(32)
		if genErr != nil {
			return fmt.Errorf("generating fallback jwt signing key: %w", genErr)
		}
		cfg.JWTSigningKey = key
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var data api.DataStore
	if cfg.InMemory {
		logger.Warn("in_memory_store_enabled", "detail", "TEAHUB_IN_MEMORY_STORE=true; repository metadata will NOT survive a restart. Never set this in production.")
		data = memstore.New()
	} else {
		db, dbErr := dbstore.Open(ctx, cfg.DatabaseURL, cfg.DBMaxOpenConns, cfg.DBMaxIdleConns, cfg.DBConnMaxLifetime)
		if dbErr != nil {
			return fmt.Errorf("connecting to database: %w", dbErr)
		}
		defer db.Close()

		migrateCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := db.Migrate(migrateCtx); err != nil {
			return fmt.Errorf("running migrations: %w", err)
		}
		logger.Info("migrations_applied")
		data = db
	}

	if err := os.MkdirAll(cfg.RepoStorageDir, 0o755); err != nil {
		return fmt.Errorf("creating repo storage dir %q: %w", cfg.RepoStorageDir, err)
	}

	cache := store.New()
	srv := api.New(cfg, data, cache, logger)
	srv.Version = version

	warmCtx, warmCancel := context.WithTimeout(ctx, 60*time.Second)
	srv.WarmCache(warmCtx)
	warmCancel()

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("server_starting", "port", cfg.Port, "env", cfg.Env)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown_signal_received")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("server error: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGracePeriod)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	logger.Info("server_stopped_cleanly")
	return nil
}

func randomKey(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
