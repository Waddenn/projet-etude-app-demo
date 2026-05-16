package metrics

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"
)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests by path and status code.",
	}, []string{"path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by path.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})

	TicketsCreatedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tickets_created_total",
		Help: "Total tickets created, by priority.",
	}, []string{"priority"})

	TicketsClosedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tickets_closed_total",
		Help: "Total tickets closed.",
	})

	TicketsOpen = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tickets_open",
		Help: "Current number of open tickets.",
	})

	DBQueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "db_query_duration_seconds",
		Help:    "Database query latency by operation.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"op"})

	JobsEnqueuedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "jobs_enqueued_total",
		Help: "Total jobs enqueued by kind.",
	}, []string{"kind"})

	JobsProcessedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "jobs_processed_total",
		Help: "Jobs processed by kind and outcome.",
	}, []string{"kind", "outcome"})

	JobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "job_duration_seconds",
		Help:    "Worker job execution latency by kind.",
		Buckets: []float64{.005, .01, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"kind"})

	JobsQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "jobs_queue_depth",
		Help: "Number of pending jobs ready to run.",
	})
)

// exemplar retourne le label trace_id à attacher au sample si un span est actif.
// Nécessite --enable-feature=exemplar-storage côté Prometheus.
func exemplar(ctx context.Context) prometheus.Labels {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return nil
	}
	return prometheus.Labels{"trace_id": sc.TraceID().String()}
}

func observeWithExemplar(ctx context.Context, h *prometheus.HistogramVec, value float64, labels ...string) {
	obs := h.WithLabelValues(labels...)
	if ex := exemplar(ctx); ex != nil {
		if e, ok := obs.(prometheus.ExemplarObserver); ok {
			e.ObserveWithExemplar(value, ex)
			return
		}
	}
	obs.Observe(value)
}

func ObserveQuery(op string, start time.Time) {
	ObserveQueryCtx(context.Background(), op, start)
}

func ObserveQueryCtx(ctx context.Context, op string, start time.Time) {
	observeWithExemplar(ctx, DBQueryDuration, time.Since(start).Seconds(), op)
}

func RecordHTTP(path string, status int, dur time.Duration) {
	RecordHTTPCtx(context.Background(), path, status, dur)
}

func RecordHTTPCtx(ctx context.Context, path string, status int, dur time.Duration) {
	observeWithExemplar(ctx, HTTPRequestDuration, dur.Seconds(), path)
	HTTPRequestsTotal.WithLabelValues(path, strconv.Itoa(status)).Inc()
}

func ObserveJobDurationCtx(ctx context.Context, kind string, dur time.Duration) {
	observeWithExemplar(ctx, JobDuration, dur.Seconds(), kind)
}
