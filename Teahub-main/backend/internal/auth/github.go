package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	githubAuthorizeURL = "https://github.com/login/oauth/authorize"
	githubTokenURL     = "https://github.com/login/oauth/access_token"
	githubUserAPIURL   = "https://api.github.com/user"
	githubEmailAPIURL  = "https://api.github.com/user/emails"
)

// GitHubOAuth implements the standard OAuth2 authorization-code flow
// against GitHub. Implemented directly against net/http rather than
// golang.org/x/oauth2: the flow GitHub requires is three plain HTTP calls
// (authorize redirect, code-for-token exchange, fetch profile) and doing
// it directly avoids a dependency for something this small.
type GitHubOAuth struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	HTTPClient   *http.Client
}

func NewGitHubOAuth(clientID, clientSecret, redirectURL string) *GitHubOAuth {
	return &GitHubOAuth{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		HTTPClient:   &http.Client{Timeout: 15 * time.Second},
	}
}

// NewState generates a cryptographically random CSRF state token to embed
// in the authorize URL and verify on callback.
func NewState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating oauth state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// AuthorizeURL builds the URL to redirect the browser to for login.
func (g *GitHubOAuth) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", g.ClientID)
	q.Set("redirect_uri", g.RedirectURL)
	q.Set("scope", "read:user user:email")
	q.Set("state", state)
	q.Set("allow_signup", "true")
	return githubAuthorizeURL + "?" + q.Encode()
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// ExchangeCode swaps an authorization code for an access token.
func (g *GitHubOAuth) ExchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{}
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", g.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchanging oauth code: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading token response: %w", err)
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if tok.Error != "" {
		return "", fmt.Errorf("github oauth error: %s (%s)", tok.Error, tok.ErrorDesc)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("github oauth: empty access token in response")
	}
	return tok.AccessToken, nil
}

// GitHubUser is the subset of GitHub's user API response TeaHub persists.
type GitHubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	Email     string `json:"email"`
}

// FetchUser retrieves the authenticated user's GitHub profile. If the
// primary profile email is private (common), it falls back to the
// /user/emails endpoint and picks the primary verified address.
func (g *GitHubOAuth) FetchUser(ctx context.Context, accessToken string) (*GitHubUser, error) {
	user, err := g.getJSON(ctx, githubUserAPIURL, accessToken, &GitHubUser{})
	if err != nil {
		return nil, err
	}
	u := user.(*GitHubUser)

	if u.Email == "" {
		if email, err := g.fetchPrimaryEmail(ctx, accessToken); err == nil {
			u.Email = email
		}
		// A missing email is non-fatal: TeaHub identifies users by GitHub
		// ID, not email, so login still succeeds without one.
	}
	return u, nil
}

func (g *GitHubOAuth) fetchPrimaryEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubEmailAPIURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&emails); err != nil {
		return "", err
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, nil
		}
	}
	return "", fmt.Errorf("no verified primary email found")
}

func (g *GitHubOAuth) getJSON(ctx context.Context, url, accessToken string, out *GitHubUser) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching github user: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("github user api returned %d: %s", resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("parsing github user response: %w", err)
	}
	return out, nil
}
