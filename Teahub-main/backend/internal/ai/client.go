// Package ai provides a shared client for calling Gemini's API (via its
// OpenAI-compatible chat completions endpoint), plus keyword-based
// retrieval for grounding prompts in a repo's actual content.
//
// Every AI-backed feature (Q&A, diagram generation, semantic search) goes
// through the same Client so there's one place that owns auth, timeouts,
// retries, model fallback, and response parsing.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// Gemini's OpenAI-compatible endpoint — same request/response shape as
	// OpenAI's Chat Completions API, so the parsing below stays simple.
	defaultAPIURL = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"

	// DefaultModel is used when GEMINI_MODEL is unset. gemini-2.0-flash was
	// shut down by Google on 2026-06-01 and gemini-2.5-flash is also being
	// retired, so the default is the current GA flash model.
	DefaultModel = "gemini-3.5-flash"

	// minMaxTokens is a floor on max_tokens. Gemini 3.x / 2.5 models are
	// "thinking" models: thinking tokens count against max_tokens, so a small
	// budget (the old 1024) can be fully consumed by thinking and come back
	// with an empty answer.
	minMaxTokens = 4096

	// maxMaxTokens caps the automatic "empty answer, retry bigger" escalation.
	maxMaxTokens = 32768
)

// fallbackModels are tried, in order, when the configured model has been
// retired / does not exist (HTTP 404 or a "model not found"-style 400), so a
// stale GEMINI_MODEL value degrades gracefully instead of breaking every
// AI feature.
var fallbackModels = []string{"gemini-3.5-flash", "gemini-3.1-flash-lite", "gemini-2.5-flash"}

// lastGoodModel remembers, per configured model name, which model actually
// worked, so we don't re-probe retired models on every request.
var lastGoodModel sync.Map // map[string]string

// Client calls Gemini's chat completions endpoint. BaseURL and HTTPClient
// are exported so tests can point at an httptest server instead of the
// real API.
type Client struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

// NewClientFromEnv builds a Client from GEMINI_API_KEY / GEMINI_MODEL.
// APIKey may be empty; callers get a clear error on first use rather than
// at construction time, so a server can start even before a key is set.
func NewClientFromEnv() *Client {
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		APIKey:     os.Getenv("GEMINI_API_KEY"),
		Model:      model,
		BaseURL:    defaultAPIURL,
		HTTPClient: &http.Client{Timeout: 50 * time.Second},
	}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// apiRequest mirrors the OpenAI Chat Completions request shape, which
// Gemini's compatibility layer accepts directly — including a "system"
// role message, unlike Anthropic's separate top-level System field.
type apiRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
}

type choice struct {
	Message message `json:"message"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Status  string `json:"status"`
}

type apiResponse struct {
	Choices []choice  `json:"choices"`
	Error   *apiError `json:"error"`
}

// CompleteOpts allows callers to override the default token budget for
// tasks that need longer output (e.g. documentation generation).
type CompleteOpts struct {
	MaxTokens int
}

// modelUnavailableError means the requested model is retired or unknown;
// Complete reacts by trying the next candidate model.
type modelUnavailableError struct {
	Model  string
	Status int
	Detail string
}

func (e *modelUnavailableError) Error() string {
	return fmt.Sprintf("model %q is unavailable (status %d): %s", e.Model, e.Status, e.Detail)
}

// Complete sends a system + user prompt to Gemini and returns the text
// response. Every feature package calls this rather than talking HTTP
// directly.
func (c *Client) Complete(ctx context.Context, system, user string, opts ...CompleteOpts) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("GEMINI_API_KEY is not set on the server")
	}
	primary := c.Model
	if primary == "" {
		primary = DefaultModel
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultAPIURL
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 50 * time.Second}
	}

	maxTokens := 1024
	if len(opts) > 0 && opts[0].MaxTokens > 0 {
		maxTokens = opts[0].MaxTokens
	}
	if maxTokens < minMaxTokens {
		maxTokens = minMaxTokens
	}

	var lastErr error
	for _, model := range candidateModels(primary) {
		text, err := c.tryModel(ctx, httpClient, baseURL, model, system, user, maxTokens)
		if err == nil {
			lastGoodModel.Store(primary, model)
			return text, nil
		}
		var unavailable *modelUnavailableError
		if errors.As(err, &unavailable) {
			lastErr = err
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("no usable Gemini model found (set GEMINI_MODEL to a current model, e.g. %s): %w", DefaultModel, lastErr)
}

// candidateModels returns the models to try, in order: the one that last
// worked for this configuration, the configured one, then the fallbacks.
func candidateModels(primary string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if v, ok := lastGoodModel.Load(primary); ok {
		add(v.(string))
	}
	add(primary)
	for _, m := range fallbackModels {
		add(m)
	}
	return out
}

// tryModel performs the request against one model, retrying once on
// transient failures (429/5xx/network) and once with a larger token budget
// if the model returned an empty answer (all tokens spent on thinking).
func (c *Client) tryModel(ctx context.Context, hc *http.Client, baseURL, model, system, user string, maxTokens int) (string, error) {
	retriedTransient, retriedTokens := false, false
	for {
		parsed, parseErr, status, body, err := doRequest(ctx, hc, baseURL, c.APIKey, model, system, user, maxTokens)
		if err != nil {
			if !retriedTransient && ctx.Err() == nil {
				retriedTransient = true
				if sleepCtx(ctx, 1500*time.Millisecond) {
					continue
				}
			}
			return "", err
		}

		if status < 400 && parseErr == nil && parsed.Error == nil && len(parsed.Choices) > 0 && parsed.Choices[0].Message.Content != "" {
			return parsed.Choices[0].Message.Content, nil
		}

		if isModelUnavailable(status, body) {
			return "", &modelUnavailableError{Model: model, Status: status, Detail: truncate(string(body), 200)}
		}
		if isTransientStatus(status) && !retriedTransient {
			retriedTransient = true
			if sleepCtx(ctx, 1500*time.Millisecond) {
				continue
			}
		}

		if msg, typ := extractAPIError(body); msg != "" {
			return "", fmt.Errorf("gemini api error (%s): %s", typ, msg)
		}
		if status >= 400 {
			return "", fmt.Errorf("gemini api returned status %d: %s", status, truncate(string(body), 300))
		}
		if parseErr != nil {
			return "", fmt.Errorf("parsing gemini response: %w (raw: %s)", parseErr, truncate(string(body), 300))
		}
		if !retriedTokens && maxTokens < maxMaxTokens {
			retriedTokens = true
			maxTokens *= 4
			if maxTokens > maxMaxTokens {
				maxTokens = maxMaxTokens
			}
			continue
		}
		return "", fmt.Errorf("empty message content from model (status %d): %s", status, truncate(string(body), 300))
	}
}

func doRequest(ctx context.Context, hc *http.Client, baseURL, apiKey, model, system, user string, maxTokens int) (parsed apiResponse, parseErr error, status int, body []byte, err error) {
	reqBody := apiRequest{
		Model:     model,
		MaxTokens: maxTokens,
		Messages: []message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return parsed, nil, 0, nil, fmt.Errorf("marshaling request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(buf))
	if err != nil {
		return parsed, nil, 0, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := hc.Do(httpReq)
	if err != nil {
		return parsed, nil, 0, nil, fmt.Errorf("calling gemini api: %w", err)
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return parsed, nil, resp.StatusCode, nil, err
	}
	parseErr = json.Unmarshal(body, &parsed)
	return parsed, parseErr, resp.StatusCode, body, nil
}

// extractAPIError pulls an error message out of either the object form
// {"error": {...}} or the array form [{"error": {...}}] that Google's
// OpenAI-compatible endpoint sometimes returns.
func extractAPIError(body []byte) (msg, typ string) {
	var one apiResponse
	if json.Unmarshal(body, &one) == nil && one.Error != nil && one.Error.Message != "" {
		return one.Error.Message, firstNonEmpty(one.Error.Type, one.Error.Status, "error")
	}
	var many []apiResponse
	if json.Unmarshal(body, &many) == nil {
		for _, r := range many {
			if r.Error != nil && r.Error.Message != "" {
				return r.Error.Message, firstNonEmpty(r.Error.Type, r.Error.Status, "error")
			}
		}
	}
	return "", ""
}

func isModelUnavailable(status int, body []byte) bool {
	if status == http.StatusNotFound {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	b := strings.ToLower(string(body))
	if !strings.Contains(b, "model") {
		return false
	}
	for _, phrase := range []string{"not found", "no longer available", "not supported", "deprecated", "retired", "is not available"} {
		if strings.Contains(b, phrase) {
			return true
		}
	}
	return false
}

func isTransientStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
