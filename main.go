package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests by path and status code.",
	}, []string{"path", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by path.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func instrument(path string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(sr, r)
		dur := time.Since(start).Seconds()
		status := strconv.Itoa(sr.status)
		httpRequestDuration.WithLabelValues(path).Observe(dur)
		httpRequestsTotal.WithLabelValues(path, status).Inc()
		slog.Info("request",
			"method", r.Method,
			"path", path,
			"status", sr.status,
			"duration_ms", dur*1000,
		)
	}
}

func helloHandler(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	fmt.Fprintf(w, "Hello from %s\n", hostname)
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", instrument("/", helloHandler))
	mux.HandleFunc("/healthz", instrument("/healthz", healthzHandler))
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	addr := ":8080"
	slog.Info("listening", "addr", addr)
	if err := http.ListenAndServe(addr, newMux()); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
