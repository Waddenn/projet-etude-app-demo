package jobworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Waddenn/projet-etude-app-demo/internal/metrics"
	"github.com/Waddenn/projet-etude-app-demo/internal/model"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
)

var tracer = otel.Tracer("projet-etude-app-demo/jobworker")

type Worker struct {
	Store      *store.Store
	Pool       *pgxpool.Pool
	ID         string
	WebhookURL string
	HTTP       *http.Client
	PollEvery  time.Duration
}

func New(s *store.Store, pool *pgxpool.Pool) *Worker {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	return &Worker{
		Store:      s,
		Pool:       pool,
		ID:         fmt.Sprintf("%s-%d", host, os.Getpid()),
		WebhookURL: os.Getenv("WEBHOOK_URL"),
		HTTP:       &http.Client{Timeout: 5 * time.Second},
		PollEvery:  5 * time.Second,
	}
}

// Run boucle jusqu'à l'annulation du contexte. Combine LISTEN 'jobs' pour le
// réveil immédiat et un poll de sécurité pour les jobs retardés (run_after futur)
// ou si un NOTIFY est perdu.
func (w *Worker) Run(ctx context.Context) error {
	conn, err := w.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listen conn: %w", err)
	}

	if _, err := conn.Exec(ctx, "LISTEN jobs"); err != nil {
		conn.Release()
		return fmt.Errorf("LISTEN: %w", err)
	}
	slog.Info("worker started", "id", w.ID)

	// Drain initial : on tente de prendre des jobs au démarrage.
	w.drain(ctx)
	w.publishDepth(ctx)

	pollTicker := time.NewTicker(w.PollEvery)
	defer pollTicker.Stop()
	depthTicker := time.NewTicker(10 * time.Second)
	defer depthTicker.Stop()

	notifCh := make(chan struct{}, 1)
	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		w.listenLoop(ctx, conn.Conn(), notifCh)
	}()

	// Release seulement après que listenLoop a terminé : sinon la
	// fermeture de la conn racerait avec WaitForNotification (cf. CI -race).
	defer func() {
		<-listenerDone
		conn.Release()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollTicker.C:
			w.drain(ctx)
		case <-notifCh:
			w.drain(ctx)
		case <-depthTicker.C:
			w.publishDepth(ctx)
		}
	}
}

func (w *Worker) listenLoop(ctx context.Context, conn *pgx.Conn, out chan<- struct{}) {
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			slog.Warn("listen wait failed", "err", err)
			time.Sleep(time.Second)
			continue
		}
		slog.Debug("notify received", "channel", n.Channel, "payload", n.Payload)
		select {
		case out <- struct{}{}:
		default:
		}
	}
}

func (w *Worker) drain(ctx context.Context) {
	for {
		job, err := w.Store.ClaimNextJob(ctx, w.ID)
		if errors.Is(err, store.ErrNotFound) {
			w.publishDepth(ctx)
			return
		}
		if err != nil {
			slog.Error("claim job", "err", err)
			return
		}
		w.process(ctx, job)
	}
}

func (w *Worker) process(ctx context.Context, j *model.Job) {
	// Restaure le contexte tracé enregistré par le producteur (api),
	// puis ouvre un span CONSUMER lié.
	ctx = extractTraceContext(ctx, j.TraceContext)
	ctx, span := tracer.Start(ctx, "job.process "+string(j.Kind),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.Int64("job.id", j.ID),
			attribute.String("job.kind", string(j.Kind)),
			attribute.Int("job.attempt", j.Attempts),
			semconv.MessagingSystemKey.String("postgresql"),
			semconv.MessagingDestinationNameKey.String("jobs"),
		),
	)
	defer span.End()

	start := time.Now()
	logger := slog.With("job_id", j.ID, "kind", string(j.Kind), "attempt", j.Attempts,
		"trace_id", span.SpanContext().TraceID().String())
	logger.Info("job started")

	err := w.execute(ctx, j)
	dur := time.Since(start)
	metrics.ObserveJobDurationCtx(ctx, string(j.Kind), dur)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.Error("job failed", "err", err, "duration_ms", dur.Milliseconds())
		metrics.JobsProcessedTotal.WithLabelValues(string(j.Kind), "error").Inc()
		if ferr := w.Store.FailJob(ctx, j.ID, j.Attempts, j.MaxAttempts, err.Error()); ferr != nil {
			logger.Error("mark fail", "err", ferr)
		}
		return
	}

	logger.Info("job done", "duration_ms", dur.Milliseconds())
	metrics.JobsProcessedTotal.WithLabelValues(string(j.Kind), "ok").Inc()
	if cerr := w.Store.CompleteJob(ctx, j.ID); cerr != nil {
		logger.Error("mark done", "err", cerr)
	}
}

func extractTraceContext(ctx context.Context, raw []byte) context.Context {
	if len(raw) == 0 {
		return ctx
	}
	var carrier propagation.MapCarrier
	if err := json.Unmarshal(raw, &carrier); err != nil || len(carrier) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

func (w *Worker) execute(ctx context.Context, j *model.Job) error {
	switch j.Kind {
	case model.JobKindWebhookNotify:
		return w.runWebhook(ctx, j.Payload)
	default:
		return fmt.Errorf("unknown job kind: %s", j.Kind)
	}
}

type webhookPayload struct {
	TicketID int64 `json:"ticket_id"`
}

func (w *Worker) runWebhook(ctx context.Context, raw []byte) error {
	var p webhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if p.TicketID == 0 {
		return errors.New("missing ticket_id")
	}

	t, err := w.Store.GetTicket(ctx, p.TicketID)
	if err != nil {
		return fmt.Errorf("load ticket: %w", err)
	}

	if w.WebhookURL == "" {
		// pas d'URL configurée : on log seulement (utile pour démo locale)
		slog.Info("webhook (stub)", "ticket_id", t.ID, "title", t.Title, "priority", t.Priority)
		return nil
	}

	body, _ := json.Marshal(map[string]any{
		"text": fmt.Sprintf("[%s] ticket #%d: %s", t.Priority, t.ID, t.Title),
		"ticket": map[string]any{
			"id":       t.ID,
			"title":    t.Title,
			"priority": t.Priority,
			"status":   t.Status,
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.WebhookURL,
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func (w *Worker) publishDepth(ctx context.Context) {
	n, err := w.Store.QueueDepth(ctx)
	if err != nil {
		slog.Warn("queue depth", "err", err)
		return
	}
	metrics.JobsQueueDepth.Set(float64(n))
}
