package main

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

	ticketsCreatedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tickets_created_total",
		Help: "Total tickets created, by priority.",
	}, []string{"priority"})

	ticketsClosedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tickets_closed_total",
		Help: "Total tickets closed.",
	})

	ticketsOpen = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tickets_open",
		Help: "Current number of open tickets.",
	})

	dbQueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "db_query_duration_seconds",
		Help:    "Database query latency by operation.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"op"})
)

func observeQuery(op string, start time.Time) {
	dbQueryDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

func recordHTTP(path string, status int, dur time.Duration) {
	httpRequestDuration.WithLabelValues(path).Observe(dur.Seconds())
	httpRequestsTotal.WithLabelValues(path, strconv.Itoa(status)).Inc()
}
