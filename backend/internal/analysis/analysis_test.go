package analysis

import (
	"context"
	"testing"
)

func TestExtractTODOs(t *testing.T) {
	content := "line one\n// TODO: fix this later\nline three\n# FIXME broken parser\nline five"
	got := extractTODOs("f.go", content)
	if len(got) != 2 {
		t.Fatalf("expected 2 TODO/FIXME markers, got %d: %+v", len(got), got)
	}
	if got[0].Name != "TODO" || got[0].Line != 2 || got[0].Signature != "fix this later" {
		t.Errorf("unexpected first marker: %+v", got[0])
	}
	if got[1].Name != "FIXME" || got[1].Line != 4 || got[1].Signature != "broken parser" {
		t.Errorf("unexpected second marker: %+v", got[1])
	}
}

func TestExtractTODOs_CaseInsensitive(t *testing.T) {
	got := extractTODOs("f.go", "// todo: lowercase works too")
	if len(got) != 1 || got[0].Name != "TODO" {
		t.Errorf("expected case-insensitive match normalized to uppercase, got %+v", got)
	}
}

func TestExtractTODOs_None(t *testing.T) {
	got := extractTODOs("f.go", "nothing to see here")
	if got != nil {
		t.Errorf("expected nil for content with no markers, got %v", got)
	}
}

func TestAnalyzeFile_DispatchesByExtension(t *testing.T) {
	cases := []struct {
		path     string
		content  string
		wantLang string
	}{
		{"main.go", "package main\n", "go"},
		{"script.py", "x = 1\n", "python"},
		{"app.js", "const x = 1;\n", "javascript"},
		{"app.ts", "const x: number = 1;\n", "typescript"},
		{"README.md", "# hello\n", "md"},
		{"noext", "hello\n", ""},
	}
	for _, c := range cases {
		fa, err := AnalyzeFile(context.Background(), c.path, c.content)
		if err != nil {
			t.Errorf("AnalyzeFile(%q): unexpected error: %v", c.path, err)
			continue
		}
		if fa.Language != c.wantLang {
			t.Errorf("AnalyzeFile(%q): expected language %q, got %q", c.path, c.wantLang, fa.Language)
		}
	}
}

func TestAnalyzeFile_AlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := AnalyzeFile(ctx, "main.go", "package main\n")
	if err == nil {
		t.Error("expected an error for an already-cancelled context")
	}
}
