// Package httpmw holds TeaHub's HTTP middleware: authentication,
// structured request logging, panic recovery, and request timeouts. Every
// handler in internal/api is wrapped with these in a fixed order (see
// api.routes) so the ordering guarantees — e.g. "a panic is always
// recovered before it can crash the process" — hold for every route
// without each handler having to opt in individually.
package httpmw

import (
	"context"
	"net/http"

	"teahub/backend/internal/auth"
	"teahub/backend/internal/dbstore"
)

type ctxKey int

const userCtxKey ctxKey = iota

// AuthedUser is the minimal identity attached to a request's context by
// RequireAuth, used by handlers to scope queries to the caller.
type AuthedUser struct {
	ID          string
	GitHubLogin string
}

// UserFromContext retrieves the authenticated user set by RequireAuth. Only
// call this from handlers mounted behind RequireAuth — it panics-free
// returns (nil, false) otherwise so misuse fails loudly in tests rather
// than serving another user's data.
func UserFromContext(ctx context.Context) (*AuthedUser, bool) {
	u, ok := ctx.Value(userCtxKey).(*AuthedUser)
	return u, ok
}

func withUser(ctx context.Context, u *AuthedUser) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

// SessionStore is the subset of *dbstore.DB the auth middleware needs,
// declared as an interface so handler tests can inject a fake instead of
// a real Postgres connection.
type SessionStore interface {
	GetUserByID(ctx context.Context, id string) (*dbstore.User, error)
	IsSessionRevoked(ctx context.Context, jwtID string) (bool, error)
}

// RequireAuth validates the session cookie (an HS256 JWT), rejects
// expired/invalid/revoked tokens with 401, loads the user, and attaches
// it to the request context for downstream handlers.
func RequireAuth(signingKey []byte, cookieName string, store SessionStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(cookieName)
			if err != nil || cookie.Value == "" {
				writeUnauthorized(w, "not signed in")
				return
			}

			claims, err := auth.VerifyJWT(signingKey, cookie.Value)
			if err != nil {
				writeUnauthorized(w, "session expired or invalid; please sign in again")
				return
			}

			revoked, err := store.IsSessionRevoked(r.Context(), claims.JWTID)
			if err != nil {
				http.Error(w, `{"error":"checking session"}`, http.StatusInternalServerError)
				return
			}
			if revoked {
				writeUnauthorized(w, "session revoked; please sign in again")
				return
			}

			user, err := store.GetUserByID(r.Context(), claims.Subject)
			if err != nil {
				writeUnauthorized(w, "user not found")
				return
			}

			ctx := withUser(r.Context(), &AuthedUser{ID: user.ID, GitHubLogin: user.GitHubLogin})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":"` + msg + `"}`))
}
