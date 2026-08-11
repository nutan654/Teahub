package ai

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const maxContextChars = 40_000 // keep prompts within a sane budget

var wordRe = regexp.MustCompile(`[a-zA-Z0-9_]+`)

type scoredFile struct {
	path  string
	score int
}

// Retrieve returns the paths of the topN files most relevant to query,
// scored by keyword overlap between query and (path + content).
//
// This is deliberately not embeddings-based (see ARCHITECTURE.md for the
// tradeoff) — it's the seam where semantic search (Stage 3 of the roadmap)
// plugs in without changing any caller.
func Retrieve(files map[string]string, query string, topN int) []string {
	terms := keywords(query)
	if len(terms) == 0 {
		return nil
	}

	scored := make([]scoredFile, 0, len(files))
	for path, content := range files {
		score := 0
		lowerPath := strings.ToLower(path)
		lowerContent := strings.ToLower(content)
		for _, t := range terms {
			score += 3 * strings.Count(lowerPath, t) // path matches weighted higher
			score += strings.Count(lowerContent, t)
		}
		if score > 0 {
			scored = append(scored, scoredFile{path, score})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })

	if len(scored) > topN {
		scored = scored[:topN]
	}
	out := make([]string, len(scored))
	for i, s := range scored {
		out[i] = s.path
	}
	return out
}

func keywords(q string) []string {
	raw := wordRe.FindAllString(strings.ToLower(q), -1)
	stop := map[string]bool{"the": true, "is": true, "a": true, "an": true, "in": true, "of": true, "to": true, "how": true, "does": true, "do": true, "where": true, "what": true, "and": true}
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		if len(w) > 2 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

// BuildContext assembles a bounded prompt context from a tree summary and
// the selected files' contents.
func BuildContext(treeSummary string, files map[string]string, paths []string) string {
	var b strings.Builder
	b.WriteString("REPOSITORY FILE TREE (top-level):\n")
	b.WriteString(treeSummary)
	b.WriteString("\n\nRELEVANT FILES:\n")
	remaining := maxContextChars
	for _, p := range paths {
		content := files[p]
		if remaining <= 0 {
			break
		}
		if len(content) > remaining {
			content = content[:remaining] + "\n... (truncated)"
		}
		fmt.Fprintf(&b, "\n--- FILE: %s ---\n%s\n", p, content)
		remaining -= len(content)
	}
	return b.String()
}
