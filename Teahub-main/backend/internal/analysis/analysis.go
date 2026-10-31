// Package analysis extracts structural information (functions, methods,
// structs/classes, interfaces, constants, imports, doc comments,
// TODO/FIXME markers) from a single file's source, per language.
//
// This is the static analysis half of TeaHub's indexing engine (see the
// index package for the concurrent, incremental layer built on top of it).
// Each language gets real parsing where a suitable parser is available in
// the Go standard library (Go itself, via go/ast) or reachable without
// adding a new runtime dependency (Python, via shelling out to python3's
// own stdlib ast module — see python.go for why that's a deliberate
// choice, not a compromise). JavaScript/TypeScript uses heuristic
// line-based regex matching, documented as such — see javascript.go.
package analysis

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
)

// SymbolKind identifies what a Symbol represents. Kept as a small closed
// set of strings (not an int enum) so it serializes to JSON exactly as
// written here, for direct use by any future API/UI layer.
type SymbolKind string

const (
	KindFunction  SymbolKind = "function"
	KindMethod    SymbolKind = "method"
	KindStruct    SymbolKind = "struct" // also covers Python/JS classes
	KindInterface SymbolKind = "interface"
	KindConstant  SymbolKind = "constant"
	KindTODO      SymbolKind = "todo"
)

// Symbol is one extracted structural element from a file.
type Symbol struct {
	Kind       SymbolKind `json:"kind"`
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	Line       int        `json:"line"`
	Signature  string     `json:"signature,omitempty"`
	DocComment string     `json:"docComment,omitempty"`
	Receiver   string     `json:"receiver,omitempty"` // set for methods: the struct/class name
	Exported   bool       `json:"exported"`
}

// FileAnalysis is everything extracted from one file.
type FileAnalysis struct {
	Path     string   `json:"path"`
	Language string   `json:"language"`
	Package  string   `json:"package,omitempty"` // package/module name, when the language has one
	Imports  []string `json:"imports,omitempty"`
	Symbols  []Symbol `json:"symbols,omitempty"`

	// Partial is true when analysis is degraded — a parse error in one
	// file, or (for Python) python3 not being available on PATH. A
	// partial result still carries whatever was extracted (e.g. TODOs are
	// always extracted regardless) rather than being discarded outright.
	Partial bool   `json:"partial,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// AnalyzeFile dispatches to the right language analyzer by file extension
// and always appends TODO/FIXME extraction (language-agnostic, comment
// syntax aside) on top of whatever the language-specific analyzer found.
//
// AnalyzeFile never returns a non-nil error for a normal parse failure —
// that's recorded as Partial+Warning on the result instead, so one
// malformed file in a repo never aborts an entire index build. The error
// return is reserved for context cancellation.
func AnalyzeFile(ctx context.Context, path, content string) (*FileAnalysis, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	var fa *FileAnalysis
	var err error
	switch ext {
	case ".go":
		fa, err = analyzeGo(path, content)
	case ".py":
		fa, err = analyzePython(ctx, path, content)
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs":
		fa, err = analyzeJS(path, content)
	default:
		fa = &FileAnalysis{Path: path, Language: unknownLanguage(ext)}
	}
	if err != nil {
		return nil, err
	}
	fa.Symbols = append(fa.Symbols, extractTODOs(path, content)...)
	return fa, nil
}

func unknownLanguage(ext string) string {
	if ext == "" {
		return ""
	}
	return strings.TrimPrefix(ext, ".")
}

// todoRe matches "TODO" or "FIXME" (any comment-syntax prefix — //, #, /*,
// <!--, etc. are all just text to this regex) followed by an optional
// colon and the rest of the line as the note.
var todoRe = regexp.MustCompile(`(?i)\b(TODO|FIXME)\b:?\s*(.*)`)

// extractTODOs scans every line of content for TODO/FIXME markers. This is
// deliberately language-agnostic (a regex over raw text, not comment-aware
// parsing) since comment syntax varies per language but the marker
// convention doesn't — and Go's own ast strips comments from doc-comment
// association in a way that would otherwise require separate handling per
// language anyway.
func extractTODOs(path, content string) []Symbol {
	var out []Symbol
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := todoRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, Symbol{
			Kind:      KindTODO,
			Name:      strings.ToUpper(m[1]),
			Path:      path,
			Line:      i + 1,
			Signature: strings.TrimSpace(m[2]),
		})
	}
	return out
}
