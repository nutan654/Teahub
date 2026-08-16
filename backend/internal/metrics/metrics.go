// Package metrics implements TeaHub's "basic request/error metrics"
// requirement with a small, dependency-free in-memory counter rather than
// a full client library (e.g. the Prometheus client): the ask was for
// counts and latency percentiles exposed over HTTP, not a metrics stack,
// and Cloud Run's own dashboards already cover CPU/memory/instance-count.
// If richer metrics (per-route breakdowns, histograms with configurable
// buckets, long-term retention) become necessary, swap this package's
// internals for a real client without changing the /metrics contract.
package metrics

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// latencyWindowSize bounds memory use: only the most recent N request
// latencies are kept, so p50/p99 reflect recent behavior rather than the
// entire process lifetime, and memory doesn't grow with uptime.
const latencyWindowSize = 2000

// Collector tracks total requests, requests by status class, 5xx error
// count, and a bounded recent-latency window for percentile calculation.
// Safe for concurrent use.
type Collector struct {
	mu sync.Mutex

	total         uint64
	errors5xx     uint64
	byStatusClass map[string]uint64

	latencies [latencyWindowSize]time.Duration
	next      int
	filled    bool
}

// New returns an empty Collector ready to use.
func New() *Collector {
	return &Collector{byStatusClass: make(map[string]uint64, 4)}
}

// Middleware records one sample (status class + latency) per request. It
// should wrap the entire handler chain, including panic recovery, so a
// recovered panic's resulting 500 is counted rather than silently
// dropped.
func (c *Collector) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		c.record(status, time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (c *Collector) record(status int, dur time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.total++
	c.byStatusClass[statusClass(status)]++
	if status >= 500 {
		c.errors5xx++
	}

	c.latencies[c.next] = dur
	c.next++
	if c.next == latencyWindowSize {
		c.next = 0
		c.filled = true
	}
}

func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// Snapshot is the JSON shape served at GET /metrics.
type Snapshot struct {
	TotalRequests      int64            `json:"totalRequests"`
	Errors5xx          int64            `json:"errors5xx"`
	RequestsByStatus   map[string]int64 `json:"requestsByStatusClass"`
	LatencyP50Ms       float64          `json:"latencyP50Ms"`
	LatencyP99Ms       float64          `json:"latencyP99Ms"`
	LatencySampleCount int              `json:"latencySampleCount"`
}

// Snapshot computes a point-in-time view of everything collected so far.
// Percentiles are computed over the current latency window at read time
// (O(n log n) in the window size, 2000 — negligible for an operator
// hitting /metrics occasionally).
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	n := latencyWindowSize
	if !c.filled {
		n = c.next
	}
	samples := make([]time.Duration, n)
	copy(samples, c.latencies[:n])

	byClass := make(map[string]int64, len(c.byStatusClass))
	for k, v := range c.byStatusClass {
		byClass[k] = int64(v)
	}
	snap := Snapshot{
		TotalRequests:      int64(c.total),
		Errors5xx:          int64(c.errors5xx),
		RequestsByStatus:   byClass,
		LatencySampleCount: n,
	}
	c.mu.Unlock()

	if n > 0 {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		snap.LatencyP50Ms = percentileMs(samples, 0.50)
		snap.LatencyP99Ms = percentileMs(samples, 0.99)
	}
	return snap
}

func percentileMs(sorted []time.Duration, p float64) float64 {
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx].Microseconds()) / 1000.0
}
