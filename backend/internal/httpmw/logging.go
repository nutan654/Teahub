package httpmw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"
)

type requestIDKey struct{}

// RequestID injects a short random request ID into the context (for log
// correlation) and echoes it back as an X-Request-Id response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the current request's ID, or "" if none.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// statusRecorder wraps ResponseWriter to capture the status code and byte
// count for access logging, since the stdlib doesn't expose either after
// the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// AccessLog logs one structured line per request: method, path, status,
// duration, and request ID. Uses log/slog so output is either
// human-readable text (dev) or JSON (prod) depending on how the logger
// passed to NewLogger was configured.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			logger.Info("http_request",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote_addr", r.RemoteAddr,
			)
		})
	}
}

// Recover catches panics in any downstream handler, logs them with a
// stack trace, and returns 500 instead of letting the process (or, worse,
// just that connection's goroutine silently) crash. Without this, an
// unexpected nil pointer deep in the analysis or diagram packages during
// a single request would take down every other in-flight request on a
// non-Cloud-Run deployment, or force a full container restart on Cloud
// Run — mid-request panics should be a 500 with a logged stack trace, not
// unclean process death.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic_recovered",
						"request_id", RequestIDFromContext(r.Context()),
						"path", r.URL.Path,
						"panic", rec,
						"stack", string(debug.Stack()),
					)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(`{"error":"internal server error"}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds handler execution with a context deadline so a slow
// downstream dependency (Anthropic API, a stuck git clone) can't hold a
// connection open indefinitely. Uses http.TimeoutHandler, which returns
// a 503 with the given message if the deadline is hit before the handler
// writes a response.
func Timeout(d time.Duration, msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, msg)
	}
}

// NewLogger builds the process-wide slog.Logger. JSON output in
// production (machine-parseable for Cloud Logging), readable text
// elsewhere.
func NewLogger(env string) *slog.Logger {
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if env == "production" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
