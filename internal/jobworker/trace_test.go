package jobworker

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Waddenn/projet-etude-app-demo/internal/model"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
	"github.com/Waddenn/projet-etude-app-demo/internal/testutil"
)

// TestTrace_EndToEndPropagation : l'api ouvre un span, enfile un job avec son
// trace_context ; le worker rejoue le span, le job.process partage le même
// TraceID, et son parent est bien le span de l'api.
func TestTrace_EndToEndPropagation(t *testing.T) {
	// In-memory tracer exporter, isolé du provider global.
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP) })

	pool := testutil.NewPostgres(t)
	st := store.New(pool)

	// Côté "api" : ouvrir un span racine, créer le ticket+job sous ce contexte.
	apiTracer := tp.Tracer("api-test")
	ctx, apiSpan := apiTracer.Start(context.Background(), "POST /api/tickets")
	_, _, err := st.CreateTicketAndEnqueue(ctx, "prod down", "", model.PriorityHigh)
	if err != nil {
		t.Fatalf("create+enqueue: %v", err)
	}
	wantTraceID := apiSpan.SpanContext().TraceID()
	apiSpan.End()

	// Vérifier que trace_context a bien été stocké en DB.
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT trace_context FROM jobs ORDER BY id DESC LIMIT 1`,
	).Scan(&raw); err != nil {
		t.Fatalf("read trace_context: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("trace_context was not persisted")
	}

	// Côté "worker" : lancer le worker, attendre le drain, puis vérifier
	// qu'un span job.process est apparu avec le même TraceID.
	w := New(st, pool)
	w.PollEvery = 100 * time.Millisecond
	runCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go w.Run(runCtx)

	deadline := time.Now().Add(6 * time.Second)
	for {
		n, err := st.QueueDepth(context.Background())
		if err != nil {
			t.Fatalf("queue depth: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue not drained")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	// Laisser le temps au span d'être exporté.
	time.Sleep(100 * time.Millisecond)

	spans := exp.GetSpans()
	var workerSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "job.process webhook.notify" {
			workerSpan = &spans[i]
			break
		}
	}
	if workerSpan == nil {
		t.Fatalf("worker span not found in exported spans; got %d spans", len(spans))
	}
	if workerSpan.SpanContext.TraceID() != wantTraceID {
		t.Fatalf("worker trace_id = %s, want %s (parent api span)",
			workerSpan.SpanContext.TraceID(), wantTraceID)
	}
	if !workerSpan.Parent.IsValid() {
		t.Fatal("worker span has no parent — propagation did not link spans")
	}
}
