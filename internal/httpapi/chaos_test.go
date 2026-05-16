package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSlow_RespectsMs(t *testing.T) {
	mux, _ := setup(t)

	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/api/slow?ms=150", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	dur := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if dur < 140*time.Millisecond {
		t.Fatalf("expected ~150ms sleep, got %v", dur)
	}
}

func TestFlaky_AllErrorsWhenRate1(t *testing.T) {
	mux, _ := setup(t)

	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/flaky?rate=1", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("iter %d: expected 500, got %d", i, rec.Code)
		}
	}
}

func TestFlaky_NeverErrorsWhenRate0(t *testing.T) {
	mux, _ := setup(t)

	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/flaky?rate=0", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d: expected 200, got %d", i, rec.Code)
		}
	}
}

// TestPanic_RecoversAndReturns500 vérifie que instrument() rattrape le panic,
// renvoie 500, et incrémente la métrique. On force chaosEnabled le temps du test.
func TestPanic_RecoversAndReturns500(t *testing.T) {
	prev := chaosEnabled
	chaosEnabled = true
	t.Cleanup(func() { chaosEnabled = prev })

	mux, _ := setup(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panic", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 after recovered panic, got %d", rec.Code)
	}

	// La métrique 500 doit être visible.
	metricsRec := httptest.NewRecorder()
	mux.ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRec.Body.String(), `http_requests_total{path="/api/panic",status="500"}`) {
		t.Fatalf("metric for /api/panic 500 not found")
	}
}

func TestChaosGuard_Disabled(t *testing.T) {
	prev := chaosEnabled
	chaosEnabled = false
	t.Cleanup(func() { chaosEnabled = prev })

	mux, _ := setup(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panic", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when chaos disabled, got %d", rec.Code)
	}
}

func TestMemleak_AllocatesAndResets(t *testing.T) {
	prev := chaosEnabled
	chaosEnabled = true
	t.Cleanup(func() {
		chaosEnabled = prev
		memHoldMu.Lock()
		memHold = nil
		memHoldMu.Unlock()
	})

	mux, _ := setup(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/memleak?mb=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	memHoldMu.Lock()
	held := len(memHold)
	memHoldMu.Unlock()
	if held == 0 {
		t.Fatal("memleak did not retain buffer")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/memleak/reset", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d", rec.Code)
	}
	memHoldMu.Lock()
	held = len(memHold)
	memHoldMu.Unlock()
	if held != 0 {
		t.Fatalf("memleak buffers not released, still %d", held)
	}
}
