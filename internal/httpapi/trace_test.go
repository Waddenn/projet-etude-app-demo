package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestOtelHttp_CreatesSpanPerRequest vérifie qu'une requête HTTP génère bien
// un span via le wrapping otelhttp + traversée jusqu'au handler.
func TestOtelHttp_CreatesSpanPerRequest(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	mux, _ := setup(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans exported — otelhttp instrumentation missing")
	}
	// Le span racine attendu est nommé d'après le pattern routé.
	found := false
	for _, s := range spans {
		if strings.Contains(s.Name, "/healthz") {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(spans))
		for _, s := range spans {
			names = append(names, s.Name)
		}
		t.Fatalf("no span named after /healthz; got %v", names)
	}
}
