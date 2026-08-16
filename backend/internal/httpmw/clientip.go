package httpmw

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the best-effort caller IP for a request, for use as a
// rate-limiter key when there's no authenticated user (e.g. Demo Mode —
// see api/demo.go). It prefers the first address in X-Forwarded-For,
// since Cloud Run (and any other reverse-proxy deployment target) always
// terminates the real client connection and forwards through a proxy —
// r.RemoteAddr alone would just be the proxy's own address for every
// caller, collapsing every visitor into a single rate-limit bucket.
//
// This is deliberately best-effort, not spoof-proof: X-Forwarded-For is
// a client-controllable header in general, and a determined abuser behind
// no proxy at all could set it to anything. That's an acceptable bound
// for what this is protecting (an anonymous demo's shared AI-cost
// budget, not an authenticated security boundary) — see
// config.DemoAskRateLimitPerMin's doc comment for the actual threat this
// defends against.
func ClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first := strings.TrimSpace(strings.Split(fwd, ",")[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
