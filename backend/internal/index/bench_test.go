package index

import (
	"context"
	"fmt"
	"testing"
)

// syntheticGoRepo generates n small-but-realistic Go files, used to
// benchmark Build/Update at a scale roughly comparable to a mid-size repo
// without needing a real clone on disk for `go test -bench` to run fast
// and deterministically in any environment (including CI).
func syntheticGoRepo(n int) map[string]string {
	files := make(map[string]string, n)
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("pkg%d/file%d.go", i%20, i)] = fmt.Sprintf(`// Package pkg%d does things.
package pkg%d

import (
	"fmt"
	"strings"
)

// MaxItems is the limit.
const MaxItems = %d

// Item represents an item.
type Item struct {
	Name string
}

// NewItem creates an item.
func NewItem(name string) Item {
	return Item{Name: name}
}

// Describe returns a description.
func (it Item) Describe() string {
	return fmt.Sprintf("item: %%s", strings.ToUpper(it.Name))
}

// TODO: add validation
func helper%d() {}
`, i%20, i%20, i, i)
	}
	return files
}

func BenchmarkBuild_100Files(b *testing.B) {
	files := syntheticGoRepo(100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Build(context.Background(), files, Options{}); err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

func BenchmarkBuild_1000Files(b *testing.B) {
	files := syntheticGoRepo(1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Build(context.Background(), files, Options{}); err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

// BenchmarkBuild_1000Files_Concurrency1 isolates the benefit of the worker
// pool by forcing serial analysis, for direct comparison against the
// default (NumCPU) concurrency used by BenchmarkBuild_1000Files.
func BenchmarkBuild_1000Files_Concurrency1(b *testing.B) {
	files := syntheticGoRepo(1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Build(context.Background(), files, Options{Concurrency: 1}); err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

// BenchmarkUpdate_NoChanges is the benchmark that matters most for the
// incremental-indexing story: re-running Update with identical content
// across 1000 files should be dramatically cheaper than a fresh Build,
// since every file is skipped by the content-hash check before ever
// reaching the worker pool.
func BenchmarkUpdate_NoChanges(b *testing.B) {
	files := syntheticGoRepo(1000)
	idx, err := Build(context.Background(), files, Options{})
	if err != nil {
		b.Fatalf("unexpected error: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.Update(context.Background(), files, nil, Options{}); err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

// BenchmarkUpdate_OnePercentChanged simulates a realistic incremental
// update after a small commit: 10 of 1000 files changed.
func BenchmarkUpdate_OnePercentChanged(b *testing.B) {
	files := syntheticGoRepo(1000)
	idx, err := Build(context.Background(), files, Options{})
	if err != nil {
		b.Fatalf("unexpected error: %v", err)
	}

	changed := make(map[string]string, len(files))
	for k, v := range files {
		changed[k] = v
	}
	i := 0
	for k := range changed {
		if i >= 10 {
			break
		}
		changed[k] = changed[k] + "\nfunc extra() {}\n"
		i++
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.Update(context.Background(), changed, nil, Options{}); err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

func BenchmarkFindByName_1000Files(b *testing.B) {
	files := syntheticGoRepo(1000)
	idx, err := Build(context.Background(), files, Options{})
	if err != nil {
		b.Fatalf("unexpected error: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.FindByName("describe")
	}
}

func BenchmarkBuild_1000Files_Concurrency8(b *testing.B) {
	files := syntheticGoRepo(1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Build(context.Background(), files, Options{Concurrency: 8}); err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}
