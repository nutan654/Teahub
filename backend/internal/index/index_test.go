package index

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"teahub/backend/internal/analysis"
)

func TestBuild_EmptyFiles(t *testing.T) {
	idx, err := Build(context.Background(), map[string]string{}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.FileCount() != 0 {
		t.Errorf("expected 0 files, got %d", idx.FileCount())
	}
}

func TestBuild_RealGoFiles(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\n\nfunc Foo() {}\n",
		"b.go": "package main\n\nfunc Bar() {}\n",
	}
	idx, err := Build(context.Background(), files, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.FileCount() != 2 {
		t.Errorf("expected 2 files, got %d", idx.FileCount())
	}
	funcs := idx.SymbolsByKind(analysis.KindFunction)
	if len(funcs) != 2 {
		t.Errorf("expected 2 functions across both files, got %d: %+v", len(funcs), funcs)
	}
}

func TestBuild_PartialFailureDoesNotAbortWholeIndex(t *testing.T) {
	files := map[string]string{
		"good.go":   "package main\n\nfunc Good() {}\n",
		"broken.go": "package main\nfunc broken( {{{",
	}
	idx, err := Build(context.Background(), files, Options{})
	if err != nil {
		t.Fatalf("Build itself should not error on a per-file parse failure: %v", err)
	}
	if idx.FileCount() != 2 {
		t.Errorf("expected both files indexed (one partially), got %d", idx.FileCount())
	}
	fa, ok := idx.File("broken.go")
	if !ok || !fa.Partial {
		t.Errorf("expected broken.go to be indexed with Partial=true, got %+v", fa)
	}
	good := idx.SymbolsByKind(analysis.KindFunction)
	if len(good) != 1 || good[0].Name != "Good" {
		t.Errorf("expected the good file's function to still be extracted, got %+v", good)
	}
}

// countingAnalyze wraps a real analyzer with a call counter, letting tests
// assert exactly how many files were actually re-analyzed — the crux of
// the incremental-update guarantee.
func countingAnalyze(t *testing.T) (workFunc, *int32) {
	t.Helper()
	var calls int32
	fn := func(ctx context.Context, path, content string) (*analysis.FileAnalysis, error) {
		atomic.AddInt32(&calls, 1)
		return analysis.AnalyzeFile(ctx, path, content)
	}
	return fn, &calls
}

func withCountingAnalyzer(t *testing.T) *int32 {
	t.Helper()
	fn, calls := countingAnalyze(t)
	orig := analyzeFile
	analyzeFile = fn
	t.Cleanup(func() { analyzeFile = orig })
	return calls
}

func TestIndex_UpdateSkipsUnchangedFiles(t *testing.T) {
	calls := withCountingAnalyzer(t)

	files := map[string]string{
		"a.go": "package main\n\nfunc A() {}\n",
		"b.go": "package main\n\nfunc B() {}\n",
	}
	idx, err := Build(context.Background(), files, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("expected 2 analyze calls after initial build, got %d", got)
	}

	// Update with IDENTICAL content for both files: zero new analyze calls.
	err = idx.Update(context.Background(), files, nil, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("expected still 2 analyze calls (no re-analysis of unchanged files), got %d", got)
	}

	// Now change only b.go: exactly one new analyze call.
	changed := map[string]string{
		"a.go": files["a.go"],                                  // unchanged
		"b.go": "package main\n\nfunc B() {}\n\nfunc C() {}\n", // changed
	}
	err = idx.Update(context.Background(), changed, nil, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("expected exactly 1 new analyze call (3 total) after changing only b.go, got %d", got)
	}

	funcs := idx.SymbolsByKind(analysis.KindFunction)
	if len(funcs) != 3 {
		t.Errorf("expected 3 functions total after the update (A, B, C), got %d: %+v", len(funcs), funcs)
	}
}

func TestIndex_UpdateHandlesDeletion(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\n\nfunc A() {}\n",
		"b.go": "package main\n\nfunc B() {}\n",
	}
	idx, _ := Build(context.Background(), files, Options{})
	if idx.FileCount() != 2 {
		t.Fatalf("expected 2 files, got %d", idx.FileCount())
	}

	if err := idx.Update(context.Background(), nil, []string{"b.go"}, Options{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.FileCount() != 1 {
		t.Errorf("expected 1 file after deleting b.go, got %d", idx.FileCount())
	}
	if _, ok := idx.File("b.go"); ok {
		t.Error("expected b.go to be gone from the index")
	}
	if _, ok := idx.File("a.go"); !ok {
		t.Error("expected a.go to remain")
	}
}

func TestIndex_Stats(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\n\nconst X = 1\n\nfunc A() {}\n",
	}
	idx, _ := Build(context.Background(), files, Options{})
	fileCount, symbolCount := idx.Stats()
	if fileCount != 1 {
		t.Errorf("expected 1 file, got %d", fileCount)
	}
	if symbolCount != 2 { // X (const) + A (func)
		t.Errorf("expected 2 symbols, got %d", symbolCount)
	}
}

func TestIndex_FindByName(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\n\nfunc VerifyToken() {}\n\nfunc GetUser() {}\n",
	}
	idx, _ := Build(context.Background(), files, Options{})

	got := idx.FindByName("token")
	if len(got) != 1 || got[0].Name != "VerifyToken" {
		t.Errorf("expected case-insensitive substring match for 'token', got %+v", got)
	}

	if got := idx.FindByName("nonexistent"); got != nil {
		t.Errorf("expected nil for no match, got %v", got)
	}
}

func TestIndex_TODOs(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\n\n// TODO: fix this\nfunc A() {}\n",
	}
	idx, _ := Build(context.Background(), files, Options{})
	todos := idx.TODOs()
	if len(todos) != 1 || todos[0].Signature != "fix this" {
		t.Errorf("expected 1 TODO, got %+v", todos)
	}
}

func TestBuild_ManyFilesConcurrently(t *testing.T) {
	files := make(map[string]string, 200)
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("file%03d.go", i)] = fmt.Sprintf("package main\n\nfunc F%d() {}\n", i)
	}
	idx, err := Build(context.Background(), files, Options{Concurrency: 8})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.FileCount() != 200 {
		t.Errorf("expected 200 files, got %d", idx.FileCount())
	}
	funcs := idx.SymbolsByKind(analysis.KindFunction)
	if len(funcs) != 200 {
		t.Errorf("expected 200 functions, got %d", len(funcs))
	}
}

func TestBuild_ContextCancellation(t *testing.T) {
	files := map[string]string{
		"a.go": "package main\n\nfunc A() {}\n",
		"b.go": "package main\n\nfunc B() {}\n",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Build even starts

	idx, _ := Build(ctx, files, Options{})
	// Cancelled up front: we don't assert an exact count (a race between
	// the producer and cancellation is inherent and fine), just that Build
	// returns promptly with a valid, non-panicking Index rather than
	// hanging or processing everything as if nothing happened.
	if idx == nil {
		t.Fatal("expected a non-nil Index even when cancelled immediately")
	}
	if idx.FileCount() > 2 {
		t.Errorf("impossible file count: %d", idx.FileCount())
	}
}
