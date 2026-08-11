// Package ai provides a shared client for calling the Claude API, plus
// keyword-based retrieval for grounding prompts in a repo's actual content.
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
	defaultAPIURL = "https://api.anthropic.com/v1/messages"
	defaultVer    = "2023-06-01"
	DefaultModel  = "claude-sonnet-5"
)

// Client calls the Claude Messages API. BaseURL and HTTPClient are exported
// so tests can point at an httptest server instead of the real API.
type Client struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

// NewClientFromEnv builds a Client from ANTHROPIC_API_KEY / ANTHROPIC_MODEL.
// APIKey may be empty; callers get a clear error on first use rather than
// at construction time, so a server can start even before a key is set.
func NewClientFromEnv() *Client {
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		APIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		Model:      model,
		BaseURL:    defaultAPIURL,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system"`
	Messages  []message `json:"messages"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type apiResponse struct {
	Content []contentBlock `json:"content"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// CompleteOpts allows callers to override the default token budget for
// tasks that need longer output (e.g. documentation generation).
type CompleteOpts struct {
	MaxTokens int
}

// Complete sends a system + user prompt to Claude and returns the text
// response. Every feature package (diagram, docs, techdebt, prreview,
// onboarding) calls this rather than talking HTTP directly.
func (c *Client) Complete(ctx context.Context, system, user string, opts ...CompleteOpts) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY is not set on the server")
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
		System:    system,
		Messages:  []message{{Role: "user", Content: user}},
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
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", defaultVer)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling anthropic api: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parsing anthropic response: %w (raw: %s)", err, string(body))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic api error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}

	var out bytes.Buffer
	for _, block := range parsed.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("empty response from model (status %d): %s", resp.StatusCode, string(body))
	}
	return out.String(), nil
}
