package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCollector_CountsByStatusClass(t *testing.T) {
	c := New()
	handler := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/bad":
			w.WriteHeader(http.StatusBadRequest)
		case "/boom":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))

	for _, path := range []string{"/ok", "/ok", "/bad", "/boom"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	snap := c.Snapshot()
	if snap.TotalRequests != 4 {
		t.Errorf("expected 4 total requests, got %d", snap.TotalRequests)
	}
	if snap.RequestsByStatus["2xx"] != 2 {
		t.Errorf("expected 2 2xx, got %d", snap.RequestsByStatus["2xx"])
	}
	if snap.RequestsByStatus["4xx"] != 1 {
		t.Errorf("expected 1 4xx, got %d", snap.RequestsByStatus["4xx"])
	}
	if snap.RequestsByStatus["5xx"] != 1 {
		t.Errorf("expected 1 5xx, got %d", snap.RequestsByStatus["5xx"])
	}
	if snap.Errors5xx != 1 {
		t.Errorf("expected 1 error, got %d", snap.Errors5xx)
	}
}

func TestCollector_NoWriteHeaderCallDefaultsTo200(t *testing.T) {
	c := New()
	handler := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok")) // never calls WriteHeader explicitly
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	snap := c.Snapshot()
	if snap.RequestsByStatus["2xx"] != 1 {
		t.Errorf("expected implicit 200 to count as 2xx, got %+v", snap.RequestsByStatus)
	}
}

func TestCollector_LatencyPercentiles(t *testing.T) {
	c := New()
	// Record 100 synthetic samples: 1ms, 2ms, ..., 100ms.
	for i := 1; i <= 100; i++ {
		c.record(200, time.Duration(i)*time.Millisecond)
	}
	snap := c.Snapshot()
	if snap.LatencySampleCount != 100 {
		t.Fatalf("expected 100 samples, got %d", snap.LatencySampleCount)
	}
	// p50 of 1..100ms should land near 50ms, p99 near 99-100ms.
	if snap.LatencyP50Ms < 45 || snap.LatencyP50Ms > 55 {
		t.Errorf("expected p50 near 50ms, got %v", snap.LatencyP50Ms)
	}
	if snap.LatencyP99Ms < 95 {
		t.Errorf("expected p99 near 99-100ms, got %v", snap.LatencyP99Ms)
	}
}

func TestCollector_WindowIsBoundedAndWrapsAround(t *testing.T) {
	c := New()
	for i := 0; i < latencyWindowSize+500; i++ {
		c.record(200, time.Millisecond)
	}
	snap := c.Snapshot()
	if snap.LatencySampleCount != latencyWindowSize {
		t.Errorf("expected window capped at %d, got %d", latencyWindowSize, snap.LatencySampleCount)
	}
	// Total request count is NOT bounded, only the latency window.
	if snap.TotalRequests != int64(latencyWindowSize+500) {
		t.Errorf("expected total requests to keep counting past window size, got %d", snap.TotalRequests)
	}
}
