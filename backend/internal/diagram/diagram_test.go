package diagram

import (
	"strings"
	"testing"

	"teahub/backend/internal/store"
)

func TestBuildSummary(t *testing.T) {
	repo := &store.Repo{
		Tree: &store.FileNode{
			Children: []*store.FileNode{
				{Path: "backend", IsDir: true},
				{Path: "frontend", IsDir: true},
				{Path: "README.md"},
			},
		},
		Files: map[string]string{
			"go.mod":                "module example",
			"backend/cmd/main.go":   "package main",
			"frontend/package.json": "{}",
		},
	}

	s := BuildSummary(repo)

	if len(s.TopLevelDirs) != 2 || s.TopLevelDirs[0] != "backend" || s.TopLevelDirs[1] != "frontend" {
		t.Errorf("unexpected top-level dirs: %v", s.TopLevelDirs)
	}
	if !contains(s.Ecosystems, "Go module") || !contains(s.Ecosystems, "Node.js/JavaScript") {
		t.Errorf("expected Go and Node ecosystems, got %v", s.Ecosystems)
	}
	if !contains(s.Entrypoints, "backend/cmd/main.go") {
		t.Errorf("expected main.go detected as entrypoint, got %v", s.Entrypoints)
	}
}

func TestBuildSummaryEmptyRepo(t *testing.T) {
	repo := &store.Repo{Tree: &store.FileNode{}, Files: map[string]string{}}
	s := BuildSummary(repo)
	str := s.String()
	if !strings.Contains(str, "(none)") && !strings.Contains(str, "(none detected)") {
		t.Errorf("expected placeholder text for empty summary, got: %s", str)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestExtractMermaidWithCodeFence(t *testing.T) {
	raw := "Here's the diagram:\n\n```mermaid\nflowchart TD\n  A --> B\n```\n\nThis shows a simple flow."
	got, err := ExtractMermaid(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "flowchart TD") {
		t.Errorf("unexpected extracted diagram: %q", got)
	}
}

func TestExtractMermaidGenericFence(t *testing.T) {
	raw := "```\ngraph LR\n  A --> B\n```"
	got, err := ExtractMermaid(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "graph LR") {
		t.Errorf("unexpected extracted diagram: %q", got)
	}
}

func TestExtractMermaidRejectsNonMermaid(t *testing.T) {
	raw := "```\nThis is just some prose, not a diagram at all.\n```"
	_, err := ExtractMermaid(raw)
	if err != ErrInvalidDiagram {
		t.Errorf("expected ErrInvalidDiagram, got %v", err)
	}
}

func TestExtractMermaidRejectsPlainProse(t *testing.T) {
	_, err := ExtractMermaid("I cannot generate a diagram for this repository.")
	if err != ErrInvalidDiagram {
		t.Errorf("expected ErrInvalidDiagram, got %v", err)
	}
}

func TestExtractMermaidSequenceDiagram(t *testing.T) {
	raw := "```mermaid\nsequenceDiagram\n  A->>B: request\n```"
	got, err := ExtractMermaid(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "sequenceDiagram") {
		t.Errorf("unexpected extracted diagram: %q", got)
	}
}
