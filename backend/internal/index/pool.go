package index

import (
	"context"
	"sync"

	"teahub/backend/internal/analysis"
)

// workFunc is the shape of analysis.AnalyzeFile — extracted as a type (and
// assigned to the package-level analyzeFile var in index.go) so tests can
// inject a counting or failing stand-in without needing real source files
// for every scenario.
type workFunc func(ctx context.Context, path, content string) (*analysis.FileAnalysis, error)

type job struct {
	path    string
	content string
}

type jobResult struct {
	path string
	fa   *analysis.FileAnalysis
	err  error
}

// runPool analyzes every entry in files concurrently across `concurrency`
// workers.
//
// Concurrency model: one producer goroutine feeds an unbuffered jobs
// channel, `concurrency` worker goroutines consume it and each send their
// result on an unbuffered results channel, and one closer goroutine closes
// results once every worker has finished (via sync.WaitGroup). runPool
// itself is both where those goroutines are spawned and where the results
// channel is fully drained — every goroutine spawned by a runPool call is
// guaranteed to have exited by the time runPool returns, cancelled or not:
//   - The producer selects on ctx.Done() when sending, so a cancelled
//     context stops it from blocking forever on a full/unread jobs channel.
//     Its deferred close(jobs) always runs, which is what lets workers'
//     `for j := range jobs` loops terminate even after the producer exits.
//   - Each worker checks ctx.Done() before starting a job and selects on
//     it when sending its result, so a cancelled context can't leave a
//     worker blocked forever on either side.
//   - The closer goroutine's only job is wg.Wait() then close(results);
//     since every worker is guaranteed to terminate (either the jobs
//     channel closes, or ctx cancels), wg.Wait() always returns.
//   - runPool's own `for r := range results` loop is the only reader of
//     results and always runs to completion, so no worker can be left
//     blocked trying to send.
//
// TestPool_NoGoroutineLeaks exercises this directly by snapshotting
// runtime.NumGoroutine() before and after a cancelled run.
func runPool(ctx context.Context, concurrency int, files map[string]string, work workFunc) (map[string]*analysis.FileAnalysis, map[string]error) {
	if concurrency < 1 {
		concurrency = 1
	}

	jobs := make(chan job)
	results := make(chan jobResult)

	go func() {
		defer close(jobs)
		for path, content := range files {
			select {
			case jobs <- job{path: path, content: content}:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				fa, err := work(ctx, j.path, j.content)
				select {
				case results <- jobResult{path: j.path, fa: fa, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	outFA := make(map[string]*analysis.FileAnalysis, len(files))
	outErr := make(map[string]error)
	for r := range results {
		if r.err != nil {
			outErr[r.path] = r.err
		} else {
			outFA[r.path] = r.fa
		}
	}
	return outFA, outErr
}
