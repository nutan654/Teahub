// Package diagram generates a Mermaid architecture diagram for an imported
// repository by summarizing its structure (not dumping full file contents —
// diagrams need shape, not detail) and asking Claude to render that shape
// as a diagram, then validating the response is actually usable Mermaid
// before it's ever shown to a user.
package diagram

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"teahub/backend/internal/ai"
	"teahub/backend/internal/store"
)

// manifestFiles map filename -> the ecosystem it signals, used to give the
// model a cheap, reliable hint about what stack it's looking at without
// having to read (and pay tokens for) full dependency files.
var manifestFiles = map[string]string{
	"go.mod": "Go module", "package.json": "Node.js/JavaScript",
	"requirements.txt": "Python (pip)", "pyproject.toml": "Python (poetry/pep517)",
	"Cargo.toml": "Rust", "pom.xml": "Java (Maven)", "build.gradle": "Java/Kotlin (Gradle)",
	"Gemfile": "Ruby", "composer.json": "PHP", "Dockerfile": "Containerized",
	"docker-compose.yml": "Multi-service (docker-compose)",
}

// StructuralSummary is a compact, deterministic description of a repo's
// shape: top-level directories, detected ecosystems, and entrypoint-like
// files. It's built without any AI call so it's fast, free, and testable
// in isolation from prompt/network concerns.
type StructuralSummary struct {
	TopLevelDirs []string
	Ecosystems   []string
	Entrypoints  []string
}

// entrypointPatterns match filenames that conventionally mark where a
// program starts, across common ecosystems.
var entrypointPatterns = regexp.MustCompile(`(?i)^(main\.go|main\.py|main\.rs|index\.(js|ts)|app\.py|server\.(js|ts|go)|manage\.py|cmd/.*\.go)$`)

// BuildSummary derives a StructuralSummary from a repo's tree and indexed files.
func BuildSummary(repo *store.Repo) StructuralSummary {
	dirSet := map[string]bool{}
	for _, c := range repo.Tree.Children {
		if c.IsDir {
			dirSet[c.Path] = true
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	ecoSet := map[string]bool{}
	entry := []string{}
	for path := range repo.Files {
		base := filepath.Base(path)
		if eco, ok := manifestFiles[base]; ok {
			ecoSet[eco] = true
		}
		if entrypointPatterns.MatchString(path) || entrypointPatterns.MatchString(base) {
			entry = append(entry, path)
		}
	}
	ecos := make([]string, 0, len(ecoSet))
	for e := range ecoSet {
		ecos = append(ecos, e)
	}
	sort.Strings(ecos)
	sort.Strings(entry)

	return StructuralSummary{TopLevelDirs: dirs, Ecosystems: ecos, Entrypoints: entry}
}

func (s StructuralSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Top-level directories: %s\n", strings.Join(orDefault(s.TopLevelDirs, "(none)"), ", "))
	fmt.Fprintf(&b, "Detected ecosystems: %s\n", strings.Join(orDefault(s.Ecosystems, "(none detected)"), ", "))
	fmt.Fprintf(&b, "Likely entrypoints: %s\n", strings.Join(orDefault(s.Entrypoints, "(none detected)"), ", "))
	return b.String()
}

func orDefault(s []string, def string) []string {
	if len(s) == 0 {
		return []string{def}
	}
	return s
}

var mermaidDiagramTypes = []string{
	"graph ", "graph\n", "flowchart ", "flowchart\n", "sequenceDiagram", "classDiagram", "erDiagram",
}

// ErrInvalidDiagram is returned when the model's response doesn't look like
// a real Mermaid diagram, so callers never forward garbage to the frontend.
var ErrInvalidDiagram = fmt.Errorf("model response is not a valid mermaid diagram")

// ExtractMermaid pulls the mermaid code block out of a model response (which
// may include prose around it) and validates it starts with a recognized
// diagram type keyword. This is deliberately strict: a diagram feature that
// silently renders broken output is worse than one that fails loudly.
func ExtractMermaid(modelOutput string) (string, error) {
	text := modelOutput
	if idx := strings.Index(text, "```mermaid"); idx != -1 {
		rest := text[idx+len("```mermaid"):]
		if end := strings.Index(rest, "```"); end != -1 {
			text = rest[:end]
		} else {
			text = rest
		}
	} else if idx := strings.Index(text, "```"); idx != -1 {
		rest := text[idx+3:]
		if end := strings.Index(rest, "```"); end != -1 {
			text = rest[:end]
		}
	}
	text = strings.TrimSpace(text)

	for _, kw := range mermaidDiagramTypes {
		if strings.HasPrefix(text, strings.TrimRight(kw, " \n")) {
			return text, nil
		}
	}
	return "", ErrInvalidDiagram
}

// Result is what Generate returns: the validated diagram plus a short
// plain-English explanation the model was also asked to produce.
type Result struct {
	Mermaid     string `json:"mermaid"`
	Explanation string `json:"explanation"`
}

const systemPrompt = "You are TeaHub's architecture diagram generator. Given a repository's " +
	"structural summary (directories, detected ecosystems, entrypoints — not full source), produce " +
	"a Mermaid diagram of its likely architecture followed by a short explanation. Never fabricate " +
	"components the structure doesn't support; if the structure is too sparse to infer much, say so " +
	"in the explanation and keep the diagram minimal rather than inventing detail."

// Generate builds a structural summary, prompts Claude for a diagram +
// explanation, and validates the diagram before returning it.
func Generate(ctx context.Context, client *ai.Client, repo *store.Repo) (*Result, error) {
	summary := BuildSummary(repo)

	prompt := "Here is the structure of a repository:\n\n" + summary.String() +
		"\n\nProduce two things, in this exact format:\n\n" +
		"1. A Mermaid diagram (flowchart or graph) showing the likely architecture " +
		"(major components/layers and how they connect), wrapped in a ```mermaid code fence. " +
		"Base it only on the structure given — do not invent components that aren't implied by it.\n" +
		"2. After the code fence, a plain-English explanation of the architecture in 3-5 sentences.\n"

	raw, err := client.Complete(ctx, systemPrompt, prompt)
	if err != nil {
		return nil, err
	}

	mermaid, err := ExtractMermaid(raw)
	if err != nil {
		return nil, fmt.Errorf("%w (raw model output: %.200s)", err, raw)
	}

	explanation := raw
	if idx := strings.LastIndex(raw, "```"); idx != -1 && idx+3 <= len(raw) {
		explanation = strings.TrimSpace(raw[idx+3:])
	}
	if explanation == "" {
		explanation = "(model did not return a separate explanation)"
	}

	return &Result{Mermaid: mermaid, Explanation: explanation}, nil
}
