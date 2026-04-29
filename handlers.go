package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	store *Store
}

func newMux(store *Store) http.Handler {
	s := &Server{store: store}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", instrument("/", s.indexHandler))
	mux.HandleFunc("GET /api/tickets", instrument("/api/tickets", s.listTicketsHandler))
	mux.HandleFunc("POST /api/tickets", instrument("/api/tickets", s.createTicketHandler))
	mux.HandleFunc("POST /api/tickets/{id}/close", instrument("/api/tickets/:id/close", s.closeTicketHandler))

	mux.HandleFunc("GET /healthz", instrument("/healthz", healthzHandler))
	mux.HandleFunc("GET /readyz", instrument("/readyz", s.readyzHandler))
	mux.HandleFunc("GET /work", instrument("/work", workHandler))

	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

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
		dur := time.Since(start)
		recordHTTP(path, sr.status, dur)
		slog.Info("request",
			"method", r.Method,
			"path", path,
			"status", sr.status,
			"duration_ms", float64(dur.Microseconds())/1000.0,
		)
	}
}

func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	tickets, err := s.store.List(r.Context(), "")
	if err != nil {
		http.Error(w, "list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	openCount, _ := s.store.CountOpen(r.Context())
	hostname, _ := os.Hostname()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "index", map[string]any{
		"Tickets":   tickets,
		"OpenCount": openCount,
		"Hostname":  hostname,
	}); err != nil {
		slog.Error("template", "err", err)
	}
}

func (s *Server) listTicketsHandler(w http.ResponseWriter, r *http.Request) {
	status := Status(r.URL.Query().Get("status"))
	tickets, err := s.store.List(r.Context(), status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tickets)
}

func (s *Server) createTicketHandler(w http.ResponseWriter, r *http.Request) {
	title, description, priority, err := parseCreate(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	t, err := s.store.Create(r.Context(), title, description, priority)
	if err != nil {
		http.Error(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ticketsCreatedTotal.WithLabelValues(string(priority)).Inc()
	if err := refreshOpenGauge(r.Context(), s.store); err != nil {
		slog.Warn("refresh open gauge", "err", err)
	}

	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = templates.ExecuteTemplate(w, "ticket-row", t)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) closeTicketHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	t, err := s.store.SetStatus(r.Context(), id, StatusClosed)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ticketsClosedTotal.Inc()
	if err := refreshOpenGauge(r.Context(), s.store); err != nil {
		slog.Warn("refresh open gauge", "err", err)
	}

	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = templates.ExecuteTemplate(w, "ticket-row", t)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func parseCreate(r *http.Request) (title, description string, priority Priority, err error) {
	priority = PriorityMedium

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var body struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Priority    string `json:"priority"`
		}
		if err = json.NewDecoder(r.Body).Decode(&body); err != nil {
			return
		}
		title, description = body.Title, body.Description
		if body.Priority != "" {
			priority = Priority(body.Priority)
		}
	} else {
		if err = r.ParseForm(); err != nil {
			return
		}
		title = r.FormValue("title")
		description = r.FormValue("description")
		if p := r.FormValue("priority"); p != "" {
			priority = Priority(p)
		}
	}

	title = strings.TrimSpace(title)
	if title == "" {
		err = errors.New("title required")
		return
	}
	if len(title) > 200 {
		err = errors.New("title too long (max 200)")
		return
	}
	if !ValidPriority(string(priority)) {
		err = errors.New("invalid priority")
		return
	}
	return
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "db unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

// workHandler simulates CPU work for `ms` milliseconds. Used to demo HPA + Kepler.
func workHandler(w http.ResponseWriter, r *http.Request) {
	ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
	if ms < 0 {
		ms = 0
	}
	if ms > 5000 {
		ms = 5000
	}
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	x := uint64(0)
	for time.Now().Before(deadline) {
		for i := 0; i < 10000; i++ {
			x = x*1103515245 + 12345
		}
	}
	_, _ = w.Write([]byte(strconv.FormatUint(x&0xff, 10) + "\n"))
}

func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if r.Header.Get("HX-Request") != "" {
		return true
	}
	if strings.Contains(accept, "text/html") && !strings.Contains(accept, "application/json") {
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
