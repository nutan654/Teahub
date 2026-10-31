package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"teahub/backend/internal/auth"
	"teahub/backend/internal/httpmw"
)

const oauthStateCookie = "teahub_oauth_state"

// handleGitHubLogin starts the OAuth flow: generates a CSRF state token,
// stores it (and sets it as a short-lived cookie as a second, stateless
// check), then redirects the browser to GitHub's authorize page.
func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		writeErr(w, http.StatusServiceUnavailable, "GitHub OAuth is not configured on this server (missing GITHUB_CLIENT_ID/SECRET)")
		return
	}
	state, err := auth.NewState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "generating oauth state")
		return
	}

	s.statesMu.Lock()
	s.states[state] = time.Now().Add(10 * time.Minute)
	s.pruneExpiredStatesLocked()
	s.statesMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.oauth.AuthorizeURL(state), http.StatusFound)
}

// pruneExpiredStatesLocked drops expired CSRF states. Caller must hold
// statesMu. Cheap linear scan; the map only ever holds a handful of
// entries (login flows complete within minutes) so this doesn't need a
// more elaborate expiry structure.
func (s *Server) pruneExpiredStatesLocked() {
	now := time.Now()
	for k, exp := range s.states {
		if now.After(exp) {
			delete(s.states, k)
		}
	}
}

// handleGitHubCallback completes the OAuth flow: validates CSRF state,
// exchanges the code for a token, fetches the GitHub profile, upserts the
// local user record, issues a session JWT, and redirects back to the
// frontend.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		writeErr(w, http.StatusServiceUnavailable, "GitHub OAuth is not configured on this server")
		return
	}

	stateParam := r.URL.Query().Get("state")
	cookie, err := r.Cookie(oauthStateCookie)
	if err != nil || stateParam == "" || cookie.Value != stateParam {
		writeErr(w, http.StatusBadRequest, "invalid or expired oauth state; please try signing in again")
		return
	}
	s.statesMu.Lock()
	exp, known := s.states[stateParam]
	delete(s.states, stateParam)
	s.statesMu.Unlock()
	if !known || time.Now().After(exp) {
		writeErr(w, http.StatusBadRequest, "invalid or expired oauth state; please try signing in again")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		writeErr(w, http.StatusBadRequest, "missing oauth code")
		return
	}

	accessToken, err := s.oauth.ExchangeCode(r.Context(), code)
	if err != nil {
		s.logger.Error("oauth_exchange_failed", "error", err)
		writeErr(w, http.StatusBadGateway, "completing GitHub sign-in failed")
		return
	}

	ghUser, err := s.oauth.FetchUser(r.Context(), accessToken)
	if err != nil {
		s.logger.Error("oauth_fetch_user_failed", "error", err)
		writeErr(w, http.StatusBadGateway, "fetching GitHub profile failed")
		return
	}

	user, err := s.data.UpsertGitHubUser(r.Context(), ghUser.ID, ghUser.Login, ghUser.Name, ghUser.AvatarURL, ghUser.Email)
	if err != nil {
		s.logger.Error("user_upsert_failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "creating local user record failed")
		return
	}

	if err := s.issueSession(w, r, user.ID); err != nil {
		s.logger.Error("issue_session_failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "starting session failed")
		return
	}

	redirectTarget := s.cfg.FrontendOrigin
	if redirectTarget == "" {
		redirectTarget = "/"
	}
	http.Redirect(w, r, redirectTarget, http.StatusFound)
}

// issueSession creates a new JWT, records it in the sessions table, and
// sets it as an HttpOnly cookie.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID string) error {
	jti, err := randomHex(16)
	if err != nil {
		return err
	}
	now := time.Now()
	expiresAt := now.Add(s.cfg.SessionTTL)

	token, err := auth.SignJWT(s.cfg.JWTSigningKey, auth.Claims{
		Subject:   userID,
		JWTID:     jti,
		IssuedAt:  now.Unix(),
		ExpiresAt: expiresAt.Unix(),
	})
	if err != nil {
		return err
	}

	if err := s.data.CreateSession(r.Context(), userID, jti, r.UserAgent(), r.RemoteAddr, expiresAt); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleLogout revokes the current session server-side (so the JWT can't
// be reused even though it hasn't expired yet) and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(s.cfg.SessionCookieName)
	if err == nil && cookie.Value != "" {
		if claims, verr := auth.VerifyJWT(s.cfg.JWTSigningKey, cookie.Value); verr == nil {
			if err := s.data.RevokeSession(r.Context(), claims.JWTID); err != nil {
				s.logger.Error("revoke_session_failed", "error", err)
			}
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

type publicUser struct {
	ID          string `json:"id"`
	GitHubLogin string `json:"githubLogin"`
}

// handleMe returns the currently authenticated user's identity, used by
// the frontend on load to decide whether to show the sign-in screen or
// the app.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := httpmw.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	writeJSON(w, http.StatusOK, publicUser{ID: u.ID, GitHubLogin: u.GitHubLogin})
}
