// Package config centralizes all environment-derived configuration so main.go
// and every other package that needs a setting reads it from one place
// instead of calling os.Getenv scattered across the codebase.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds every environment-derived setting TeaHub needs to run.
type Config struct {
	// Port is the HTTP port the server listens on.
	Port string

	// Env is a free-form deployment environment name ("development",
	// "production", "test"). Only used for log formatting decisions and
	// for a couple of "don't allow insecure defaults in prod" checks.
	Env string

	// DatabaseURL is a standard postgres:// connection string. Required
	// in all environments except when TEAHUB_IN_MEMORY_STORE=true, which
	// exists only to keep `go run` friction-free for local hacking on
	// non-persistence-related code; it must never be set in production.
	DatabaseURL string
	InMemory    bool

	// DBMaxOpenConns / DBMaxIdleConns / DBConnMaxLifetime tune the
	// database/sql connection pool. Defaults are conservative for a
	// single Cloud Run instance talking to Cloud SQL.
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration

	// AnthropicAPIKey / AnthropicModel configure the AI client. Empty is
	// allowed at startup (server still boots; /api/ask fails until set).
	AnthropicAPIKey string
	AnthropicModel  string

	// GitHubClientID / GitHubClientSecret / GitHubOAuthRedirectURL configure
	// the "Sign in with GitHub" OAuth flow.
	GitHubClientID     string
	GitHubClientSecret string
	GitHubRedirectURL  string

	// JWTSigningKey signs session JWTs (HS256). Must be at least 32 bytes
	// in production; a random key is generated at startup in dev/test if
	// unset so local iteration doesn't require configuring one, but that
	// means sessions won't survive a restart in that mode.
	JWTSigningKey []byte

	// SessionCookieName / SessionTTL / SecureCookies control the session
	// cookie TeaHub issues after a successful OAuth login.
	SessionCookieName string
	SessionTTL        time.Duration
	SecureCookies     bool

	// FrontendOrigin is the origin allowed for CORS + the OAuth redirect
	// target after login (e.g. https://teahub.example.com). Empty means
	// "same origin as the API" (the common case: Go serves the SPA too).
	FrontendOrigin string

	// RepoStorageDir is where cloned repository working trees live on
	// disk. Repository *metadata* lives in Postgres; this directory only
	// holds file bodies, which are never written to the database. See
	// ARCHITECTURE.md "Storage model" for why, and for the Cloud Run
	// caveat (local disk there is ephemeral per-instance, so repos are
	// re-cloned on demand if missing rather than assumed durable).
	RepoStorageDir string

	// RequestTimeout bounds how long any single HTTP handler may run
	// (excluding streaming responses), enforced via context in the
	// timeout middleware.
	RequestTimeout time.Duration

	// ShutdownGracePeriod is how long the server waits for in-flight
	// requests to finish after receiving SIGTERM/SIGINT before forcing
	// the listener closed.
	ShutdownGracePeriod time.Duration

	// MaxRequestBodyBytes caps the raw request body size accepted by
	// cost-bearing endpoints (import/ask/search), enforced via
	// http.MaxBytesReader before JSON decoding even begins.
	MaxRequestBodyBytes int64

	// MaxQuestionLength / MaxSearchQueryLength cap the length (in runes)
	// of the free-text fields that get forwarded to the Anthropic API, so
	// a single request can't balloon token spend.
	MaxQuestionLength    int
	MaxSearchQueryLength int

	// ImportRateLimitPerMin / ImportRateLimitBurst and AskRateLimitPerMin
	// / AskRateLimitBurst configure the per-user token-bucket limiters on
	// /api/import and /api/ask (and /api/repos/{id}/search, which shares
	// the ask limiter since it's equally cost-bearing).
	ImportRateLimitPerMin float64
	ImportRateLimitBurst  int
	AskRateLimitPerMin    float64
	AskRateLimitBurst     int
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func getenvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// Load reads configuration from the environment. It returns an error only
// for combinations that would be unsafe or non-functional to run with (e.g.
// production without a database or without a real JWT key); everything
// else falls back to a sane development default.
func Load() (*Config, error) {
	env := getenv("TEAHUB_ENV", "development")

	cfg := &Config{
		Port:                getenv("PORT", "8080"),
		Env:                 env,
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		InMemory:            getenvBool("TEAHUB_IN_MEMORY_STORE", false),
		DBMaxOpenConns:      getenvInt("DB_MAX_OPEN_CONNS", 10),
		DBMaxIdleConns:      getenvInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLifetime:   getenvDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		AnthropicAPIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:      os.Getenv("ANTHROPIC_MODEL"),
		GitHubClientID:      os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret:  os.Getenv("GITHUB_CLIENT_SECRET"),
		GitHubRedirectURL:   os.Getenv("GITHUB_OAUTH_REDIRECT_URL"),
		SessionCookieName:   getenv("SESSION_COOKIE_NAME", "teahub_session"),
		SessionTTL:          getenvDuration("SESSION_TTL", 7*24*time.Hour),
		SecureCookies:       getenvBool("SECURE_COOKIES", env == "production"),
		FrontendOrigin:      os.Getenv("FRONTEND_ORIGIN"),
		RepoStorageDir:      getenv("REPO_STORAGE_DIR", "./data/repos"),
		RequestTimeout:      getenvDuration("REQUEST_TIMEOUT", 60*time.Second),
		ShutdownGracePeriod: getenvDuration("SHUTDOWN_GRACE_PERIOD", 15*time.Second),

		MaxRequestBodyBytes:   int64(getenvInt("MAX_REQUEST_BODY_BYTES", 1<<20)), // 1 MiB
		MaxQuestionLength:     getenvInt("MAX_QUESTION_LENGTH", 4000),
		MaxSearchQueryLength:  getenvInt("MAX_SEARCH_QUERY_LENGTH", 500),
		ImportRateLimitPerMin: getenvFloat("IMPORT_RATE_LIMIT_PER_MIN", 5),
		ImportRateLimitBurst:  getenvInt("IMPORT_RATE_LIMIT_BURST", 3),
		AskRateLimitPerMin:    getenvFloat("ASK_RATE_LIMIT_PER_MIN", 20),
		AskRateLimitBurst:     getenvInt("ASK_RATE_LIMIT_BURST", 10),
	}

	if key := os.Getenv("JWT_SIGNING_KEY"); key != "" {
		cfg.JWTSigningKey = []byte(key)
	}

	if env == "production" {
		if cfg.DatabaseURL == "" && !cfg.InMemory {
			return nil, fmt.Errorf("DATABASE_URL is required when TEAHUB_ENV=production")
		}
		if len(cfg.JWTSigningKey) < 32 {
			return nil, fmt.Errorf("JWT_SIGNING_KEY must be set and at least 32 bytes in production")
		}
		if cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "" {
			return nil, fmt.Errorf("GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET are required when TEAHUB_ENV=production")
		}
	}

	if cfg.DatabaseURL == "" && !cfg.InMemory {
		// Local dev convenience default matching docker-compose.yml.
		cfg.DatabaseURL = "postgres://teahub:teahub@localhost:5432/teahub?sslmode=disable"
	}

	return cfg, nil
}
