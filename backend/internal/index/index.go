// Package index is TeaHub's incremental indexing engine: it turns a repo's
// files into a queryable model of its structure (packages, functions,
// methods, types, constants, TODOs — see the analysis package for what's
// actually extracted per file) and keeps that model current without ever
// re-walking or re-analyzing files that haven't changed.
//
// The design goal stated in ARCHITECTURE.md's indexing section: a repo is
// indexed once during import (Build), and every later operation — a
// search, an explorer view, an incremental re-index after new commits —
// works against the already-built Index rather than re-reading the
// repository from disk. See ARCHITECTURE.md for the full complexity
// analysis and concurrency model writeup.
package index

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"

	"teahub/backend/internal/analysis"
)

// Options configures a Build or Update call.
type Options struct {
	// Concurrency is how many files are analyzed in parallel. Zero (the
	// default) means runtime.NumCPU() — analysis is CPU-bound (parsing) or
	// short-lived-subprocess-bound (Python), so tying it to core count is
	// the right default; tests and callers that want deterministic
	// behavior can set it explicitly.
	Concurrency int
}

func (o Options) concurrency() int {
	if o.Concurrency > 0 {
		return o.Concurrency
	}
	return runtime.NumCPU()
}

// Index holds the analyzed state of every file in a repo. Safe for
// concurrent use: Build/Update take a write lock, every query method takes
// a read lock, so queries (e.g. from an HTTP handler) never block on each
// other and only briefly block on an in-progress Update.
type Index struct {
	mu     sync.RWMutex
	files  map[string]*analysis.FileAnalysis
	hashes map[string]string // path -> content hash, for incremental diffing in Update
}

// New returns an empty Index. Exported mainly for tests; production code
// gets an already-built Index from Build.
func New() *Index {
	return &Index{files: map[string]*analysis.FileAnalysis{}, hashes: map[string]string{}}
}

// analyzeFile is a package-level indirection to analysis.AnalyzeFile, not a
// direct reference to it — this is the seam tests use to inject a
// counting or synthetic work function (see index_test.go) without needing
// real Go/Python/JS source for every test case.
var analyzeFile workFunc = analysis.AnalyzeFile

// Build performs the one and only full walk of files (path -> content):
// every file is analyzed concurrently, and the result is a ready-to-query
// Index. This is the "indexed once during import" half of the incremental
// guarantee — see Update for the "never re-walked during search" half.
func Build(ctx context.Context, files map[string]string, opts Options) (*Index, error) {
	idx := New()
	if len(files) == 0 {
		return idx, nil
	}

	results, errs := runPool(ctx, opts.concurrency(), files, analyzeFile)

	idx.mu.Lock()
	for path, fa := range results {
		idx.files[path] = fa
		idx.hashes[path] = hashContent(files[path])
	}
	idx.mu.Unlock()

	return idx, combineErrors(errs)
}

// Update re-analyzes only the files in changed whose content hash differs
// from what's already indexed, and removes every path in deleted. A file
// present in changed with byte-identical content to what's already stored
// is never passed to the worker pool at all — this is the incremental
// guarantee itself, and TestIndex_UpdateSkipsUnchangedFiles asserts it by
// counting actual analyze calls, not just checking output correctness.
func (idx *Index) Update(ctx context.Context, changed map[string]string, deleted []string, opts Options) error {
	toAnalyze := make(map[string]string)
	idx.mu.RLock()
	for path, content := range changed {
		if idx.hashes[path] != hashContent(content) {
			toAnalyze[path] = content
		}
	}
	idx.mu.RUnlock()

	var buildErr error
	if len(toAnalyze) > 0 {
		results, errs := runPool(ctx, opts.concurrency(), toAnalyze, analyzeFile)
		idx.mu.Lock()
		for path, fa := range results {
			idx.files[path] = fa
			idx.hashes[path] = hashContent(toAnalyze[path])
		}
		idx.mu.Unlock()
		buildErr = combineErrors(errs)
	}

	if len(deleted) > 0 {
		idx.mu.Lock()
		for _, path := range deleted {
			delete(idx.files, path)
			delete(idx.hashes, path)
		}
		idx.mu.Unlock()
	}

	return buildErr
}

// BuildError reports which files failed analysis during a Build or Update.
// A file failing analysis never aborts the whole call (see runPool) — this
// is returned alongside a fully-usable Index for whatever did succeed.
type BuildError struct {
	Failed []string
	Errs   map[string]error
}

func (e *BuildError) Error() string {
	return fmt.Sprintf("%d file(s) failed analysis: %s", len(e.Failed), strings.Join(e.Failed, ", "))
}

func combineErrors(errs map[string]error) error {
	if len(errs) == 0 {
		return nil
	}
	paths := make([]string, 0, len(errs))
	for p := range errs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return &BuildError{Failed: paths, Errs: errs}
}

// FileCount returns how many files are currently indexed.
func (idx *Index) FileCount() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.files)
}

// Stats returns the total indexed file count and total extracted symbol
// count across the whole repo.
func (idx *Index) Stats() (fileCount, symbolCount int) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	fileCount = len(idx.files)
	for _, fa := range idx.files {
		symbolCount += len(fa.Symbols)
	}
	return fileCount, symbolCount
}

// File returns the analysis for one indexed path.
func (idx *Index) File(path string) (*analysis.FileAnalysis, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	fa, ok := idx.files[path]
	return fa, ok
}

// SymbolsByKind returns every symbol of the given kind across the whole
// repo, sorted by path then line for stable, readable output.
func (idx *Index) SymbolsByKind(kind analysis.SymbolKind) []analysis.Symbol {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var out []analysis.Symbol
	for _, fa := range idx.files {
		for _, s := range fa.Symbols {
			if s.Kind == kind {
				out = append(out, s)
			}
		}
	}
	sortSymbols(out)
	return out
}

// TODOs is shorthand for SymbolsByKind(analysis.KindTODO) — the common
// case of wanting every TODO/FIXME marker across a repo.
func (idx *Index) TODOs() []analysis.Symbol {
	return idx.SymbolsByKind(analysis.KindTODO)
}

// FindByName does a case-insensitive substring search over symbol names
// across every indexed file — the lookup primitive a future Symbol
// Navigation feature builds on ("go to definition"-style lookup by name).
func (idx *Index) FindByName(query string) []analysis.Symbol {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	q := strings.ToLower(query)
	var out []analysis.Symbol
	for _, fa := range idx.files {
		for _, s := range fa.Symbols {
			if strings.Contains(strings.ToLower(s.Name), q) {
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func sortSymbols(s []analysis.Symbol) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Path != s[j].Path {
			return s[i].Path < s[j].Path
		}
		return s[i].Line < s[j].Line
	})
}
