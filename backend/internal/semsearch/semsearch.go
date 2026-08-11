// Package semsearch implements "semantic" code search without a vector
// database or embeddings provider — neither is reachable from every
// deployment environment, and pulling one in for a single feature would
// violate the "avoid unnecessary dependencies" goal for this project.
//
// Instead this is a hybrid, and an honest one: a cheap keyword pre-filter
// narrows a large repo down to a candidate pool (so we never pay to send
// an entire codebase to the model), then Claude re-ranks that pool by
// actual semantic relevance to the query — catching matches that share no
// literal words with the query (e.g. "prevent replay attacks" matching a
// nonce-checking function that never uses the word "replay"). Every path
// the model returns is validated against the real candidate set before
// it's ever shown to a user, so a hallucinated file path can't leak out.
//
// This is the seam described in ARCHITECTURE.md: swapping in real
// embeddings + a vector store later means replacing buildCandidates below,
// not anything that calls Search.
package semsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"teahub/backend/internal/ai"
	"teahub/backend/internal/store"
)

// maxCandidates bounds how many files we ever describe to the model in one
// call, keeping prompt size (and cost) predictable regardless of repo size.
const maxCandidates = 60

// snippetChars is how much of each candidate file's content we show the
// model — enough to judge relevance, not enough to blow the token budget
// across 60 files.
const snippetChars = 300

// Result is one ranked match.
type Result struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// buildCandidates selects which files are even considered for a query.
// Pure logic, no network — fully unit-testable in isolation.
//
// Strategy: start with keyword-retrieved files (cheap, precise when terms
// overlap), then fill remaining budget with other indexed files so a
// query with zero literal keyword overlap still has a full pool for the
// model's semantic judgment to work over, rather than an empty one.
func buildCandidates(files map[string]string, query string) []string {
	keywordHits := ai.Retrieve(files, query, maxCandidates)
	seen := make(map[string]bool, len(keywordHits))
	candidates := make([]string, 0, maxCandidates)
	for _, p := range keywordHits {
		candidates = append(candidates, p)
		seen[p] = true
	}
	if len(candidates) >= maxCandidates {
		return candidates
	}

	rest := make([]string, 0, len(files))
	for p := range files {
		if !seen[p] {
			rest = append(rest, p)
		}
	}
	sort.Strings(rest) // deterministic order so results/tests are stable
	for _, p := range rest {
		if len(candidates) >= maxCandidates {
			break
		}
		candidates = append(candidates, p)
	}
	return candidates
}

func buildPrompt(candidates []string, files map[string]string, query string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "QUERY: %s\n\nCANDIDATE FILES (path — snippet):\n", query)
	for _, p := range candidates {
		content := files[p]
		if len(content) > snippetChars {
			content = content[:snippetChars] + "..."
		}
		content = strings.ReplaceAll(content, "\n", " ")
		fmt.Fprintf(&b, "\n- %s — %s", p, content)
	}
	b.WriteString("\n\nReturn the files most semantically relevant to the query, even if they " +
		"don't share literal words with it. Respond with ONLY a JSON array (no prose, no code " +
		"fence), up to 10 items, ordered most to least relevant, each shaped exactly like:\n" +
		`[{"path": "exact/path/from/candidates", "reason": "one short sentence"}]` +
		"\n\nUse only paths that appear verbatim in the candidate list above. If nothing is " +
		"relevant, return [].")
	return b.String()
}

const systemPrompt = "You are TeaHub's semantic code search engine. You judge relevance by " +
	"meaning, not keyword overlap. Never invent a file path that wasn't given to you."

// ExtractJSONArray pulls a JSON array out of a model response that may
// include prose or a code fence around it. Exported so it's directly
// unit-testable against the many shapes models actually return.
func ExtractJSONArray(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if idx := strings.Index(text, "```"); idx != -1 {
		rest := text[idx:]
		rest = strings.TrimPrefix(rest, "```json")
		rest = strings.TrimPrefix(rest, "```")
		if end := strings.Index(rest, "```"); end != -1 {
			text = strings.TrimSpace(rest[:end])
		}
	}
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start == -1 || end == -1 || end < start {
		return "", fmt.Errorf("no JSON array found in model response")
	}
	return text[start : end+1], nil
}

// Search runs hybrid semantic search over an imported repo's indexed files.
func Search(ctx context.Context, client *ai.Client, repo *store.Repo, query string) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	if len(repo.Files) == 0 {
		return []Result{}, nil
	}

	candidates := buildCandidates(repo.Files, query)
	candidateSet := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candidateSet[c] = true
	}

	prompt := buildPrompt(candidates, repo.Files, query)
	raw, err := client.Complete(ctx, systemPrompt, prompt, ai.CompleteOpts{MaxTokens: 1024})
	if err != nil {
		return nil, err
	}

	arr, err := ExtractJSONArray(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing search results: %w (raw model output: %.200s)", err, raw)
	}

	var parsed []Result
	if err := json.Unmarshal([]byte(arr), &parsed); err != nil {
		return nil, fmt.Errorf("invalid JSON from model: %w", err)
	}

	// Validate every result against the real candidate set — this is the
	// hard guarantee: a hallucinated path never reaches the caller.
	out := make([]Result, 0, len(parsed))
	for _, r := range parsed {
		if candidateSet[r.Path] {
			out = append(out, r)
		}
	}
	return out, nil
}
