// Package ai provides a shared client for calling Gemini's free-tier API
// (via its OpenAI-compatible chat completions endpoint), plus keyword-based
// retrieval for grounding prompts in a repo's actual content.
//
// Every AI-backed feature (Q&A, diagram generation, doc generation,
// tech-debt analysis, PR review) goes through the same Client so there's
// one place that owns auth, timeouts, retries, and response parsing.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	// Gemini's OpenAI-compatible endpoint — same request/response shape as
	// OpenAI's Chat Completions API, so the parsing below stays simple.
	defaultAPIURL = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	DefaultModel  = "gemini-2.0-flash"
)

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
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
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

type apiResponse struct {
	Choices []choice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// CompleteOpts allows callers to override the default token budget for
// tasks that need longer output (e.g. documentation generation).
type CompleteOpts struct {
	MaxTokens int
}

// Complete sends a system + user prompt to Gemini and returns the text
// response. Every feature package (diagram, docs, techdebt, prreview,
// onboarding) calls this rather than talking HTTP directly.
func (c *Client) Complete(ctx context.Context, system, user string, opts ...CompleteOpts) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("GEMINI_API_KEY is not set on the server")
	}
	model := c.Model
	if model == "" {
		model = DefaultModel
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultAPIURL
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}

	maxTokens := 1024
	if len(opts) > 0 && opts[0].MaxTokens > 0 {
		maxTokens = opts[0].MaxTokens
	}

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
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling gemini api: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parsing gemini response: %w (raw: %s)", err, string(body))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("gemini api error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("empty response from model (status %d): %s", resp.StatusCode, string(body))
	}

	return parsed.Choices[0].Message.Content, nil
}
