package index

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"teahub/backend/internal/analysis"
)

func TestRunPool_AllFilesProcessed(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 50; i++ {
		files[fmt.Sprintf("f%d.go", i)] = fmt.Sprintf("package main\nfunc F%d() {}\n", i)
	}
	work := func(ctx context.Context, path, content string) (*analysis.FileAnalysis, error) {
		return &analysis.FileAnalysis{Path: path}, nil
	}
	results, errs := runPool(context.Background(), 4, files, work)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(results) != 50 {
		t.Fatalf("expected 50 results, got %d", len(results))
	}
}

func TestRunPool_RespectsMaxConcurrency(t *testing.T) {
	const concurrency = 3
	var current, maxSeen int32
	var mu sync.Mutex

	files := map[string]string{}
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("f%d.go", i)] = "x"
	}

	work := func(ctx context.Context, path, content string) (*analysis.FileAnalysis, error) {
		n := atomic.AddInt32(&current, 1)
		mu.Lock()
		if n > maxSeen {
			maxSeen = n
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond) // hold the "slot" long enough to observe overlap
		atomic.AddInt32(&current, -1)
		return &analysis.FileAnalysis{Path: path}, nil
	}

	runPool(context.Background(), concurrency, files, work)

	if maxSeen > concurrency {
		t.Errorf("observed %d concurrent workers, expected at most %d", maxSeen, concurrency)
	}
	if maxSeen < 2 {
		t.Errorf("expected genuine concurrency (>1 worker overlapping), observed max %d — test may be unreliable in this environment", maxSeen)
	}
}

func TestRunPool_ErrorsCollectedSeparately(t *testing.T) {
	files := map[string]string{"good.go": "ok", "bad.go": "fail"}
	work := func(ctx context.Context, path, content string) (*analysis.FileAnalysis, error) {
		if content == "fail" {
			return nil, fmt.Errorf("boom")
		}
		return &analysis.FileAnalysis{Path: path}, nil
	}
	results, errs := runPool(context.Background(), 2, files, work)
	if len(results) != 1 || results["good.go"] == nil {
		t.Errorf("expected good.go in results, got %v", results)
	}
	if len(errs) != 1 || errs["bad.go"] == nil {
		t.Errorf("expected bad.go in errs, got %v", errs)
	}
}

func TestRunPool_ZeroOrNegativeConcurrencyDefaultsToOne(t *testing.T) {
	files := map[string]string{"a.go": "x"}
	work := func(ctx context.Context, path, content string) (*analysis.FileAnalysis, error) {
		return &analysis.FileAnalysis{Path: path}, nil
	}
	results, _ := runPool(context.Background(), 0, files, work)
	if len(results) != 1 {
		t.Errorf("expected 1 result even with concurrency=0, got %d", len(results))
	}
	results, _ = runPool(context.Background(), -5, files, work)
	if len(results) != 1 {
		t.Errorf("expected 1 result even with concurrency=-5, got %d", len(results))
	}
}

// TestRunPool_NoGoroutineLeaks is the direct evidence for the "no goroutine
// leaks" requirement: it snapshots the goroutine count, runs a pool against
// slow work with a context that gets cancelled partway through, and asserts
// the goroutine count returns to baseline shortly after runPool returns.
func TestRunPool_NoGoroutineLeaks(t *testing.T) {
	before := runtime.NumGoroutine()

	files := map[string]string{}
	for i := 0; i < 100; i++ {
		files[fmt.Sprintf("f%d.go", i)] = "x"
	}

	ctx, cancel := context.WithCancel(context.Background())
	var started int32
	work := func(ctx context.Context, path, content string) (*analysis.FileAnalysis, error) {
		n := atomic.AddInt32(&started, 1)
		if n == 5 {
			cancel() // cancel partway through, once a few jobs are in flight
		}
		time.Sleep(time.Millisecond)
		return &analysis.FileAnalysis{Path: path}, nil
	}

	runPool(ctx, 8, files, work)
	cancel() // no-op if already cancelled; ensures cleanup regardless of timing

	// Goroutine counts can be noisy immediately after a run (GC workers,
	// runtime bookkeeping), so poll briefly rather than asserting instantly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		after := runtime.NumGoroutine()
		if after <= before+2 { // small slack for unrelated runtime goroutines
			return // success
		}
		if time.Now().After(deadline) {
			t.Errorf("goroutine leak suspected: before=%d after=%d", before, after)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunPool_CancelledContextStillReturnsPromptly(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 500; i++ {
		files[fmt.Sprintf("f%d.go", i)] = "x"
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before we even start

	work := func(ctx context.Context, path, content string) (*analysis.FileAnalysis, error) {
		time.Sleep(50 * time.Millisecond) // would take 25s+ serially if not short-circuited
		return &analysis.FileAnalysis{Path: path}, nil
	}

	done := make(chan struct{})
	go func() {
		runPool(ctx, 4, files, work)
		close(done)
	}()

	select {
	case <-done:
		// good: returned promptly instead of processing all 500 slow jobs
	case <-time.After(3 * time.Second):
		t.Fatal("runPool did not return promptly for an already-cancelled context")
	}
}
