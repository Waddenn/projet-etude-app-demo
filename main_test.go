package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	healthzHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Fatalf("expected body 'ok', got %q", got)
	}
}

func TestHello(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	helloHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), "Hello from ") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

func TestStatusRecorder(t *testing.T) {
	sr := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	sr.WriteHeader(http.StatusTeapot)
	if sr.status != http.StatusTeapot {
		t.Fatalf("expected 418, got %d", sr.status)
	}
}

func TestInstrumentWraps(t *testing.T) {
	called := false
	wrapped := instrument("/x", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	})
	rec := httptest.NewRecorder()
	wrapped(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if !called {
		t.Fatal("inner handler not called")
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
}

func TestMuxRoutes(t *testing.T) {
	mux := newMux()
	cases := []struct {
		path string
		code int
		body string
	}{
		{"/", http.StatusOK, "Hello from "},
		{"/healthz", http.StatusOK, "ok"},
		{"/metrics", http.StatusOK, "http_requests_total"},
	}
	// Hit instrumented routes once first so /metrics has data to expose.
	for _, p := range []string{"/", "/healthz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.code {
			t.Errorf("%s: expected %d, got %d", c.path, c.code, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: body missing %q, got %q", c.path, c.body, rec.Body.String())
		}
	}
}
