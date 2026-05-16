package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"

	"github.com/Waddenn/projet-etude-app-demo/internal/auth"
	"github.com/Waddenn/projet-etude-app-demo/internal/metrics"
	"github.com/Waddenn/projet-etude-app-demo/internal/model"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
)

type Server struct {
	Store *store.Store
	Auth  *auth.Authenticator
	Login *auth.Login
}

func NewMux(s *store.Store, a *auth.Authenticator, l *auth.Login) http.Handler {
	srv := &Server{Store: s, Auth: a, Login: l}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", instrument("/", srv.indexHandler))
	mux.HandleFunc("GET /api/tickets", instrument("/api/tickets", auth.RequireRead(srv.listTicketsHandler)))
	mux.HandleFunc("POST /api/tickets", instrument("/api/tickets",
		auth.RequireMutate(srv.auditing("ticket.create", srv.createTicketHandler))))
	mux.HandleFunc("POST /api/tickets/{id}/close", instrument("/api/tickets/:id/close",
		auth.RequireMutate(srv.auditing("ticket.close", srv.closeTicketHandler))))

	mux.HandleFunc("GET /api/audit", instrument("/api/audit", auth.RequireMutate(srv.listAuditHandler)))

	// Publics : probes K8s, scrape Prometheus, login flow.
	mux.HandleFunc("GET /healthz", instrument("/healthz", healthzHandler))
	mux.HandleFunc("GET /readyz", instrument("/readyz", srv.readyzHandler))
	mux.HandleFunc("GET /work", instrument("/work", workHandler))
	if l != nil {
		mux.HandleFunc("GET /auth/login", instrument("/auth/login", l.StartHandler))
		mux.HandleFunc("GET /auth/callback", instrument("/auth/callback", l.CallbackHandler))
		mux.HandleFunc("GET /auth/logout", instrument("/auth/logout", l.LogoutHandler))
	}

	// Endpoints SLI / chaos. Les destructifs sont gardés par DEMO_CHAOS_ENABLED.
	mux.HandleFunc("GET /api/slow", instrument("/api/slow", slowHandler))
	mux.HandleFunc("GET /api/flaky", instrument("/api/flaky", flakyHandler))
	mux.HandleFunc("GET /api/panic", instrument("/api/panic", chaosGuard(panicHandler)))
	mux.HandleFunc("GET /api/crash", instrument("/api/crash", chaosGuard(crashHandler)))
	mux.HandleFunc("GET /api/memleak", instrument("/api/memleak", chaosGuard(memleakHandler)))
	mux.HandleFunc("POST /api/memleak/reset", instrument("/api/memleak/reset", chaosGuard(memleakResetHandler)))

	mux.Handle("GET /metrics", promhttp.Handler())

	// Pipeline : OTel HTTP → auth middleware → mux. L'auth doit voir le span
	// pour que les logs / audits sortent avec le trace_id correct.
	var handler http.Handler = mux
	if a != nil {
		handler = a.Middleware(handler)
	}

	// otelhttp englobe le mux : un span par requête, nom = pattern routé
	// (ex. "POST /api/tickets"). Le contexte tracé est dispo via r.Context()
	// dans tous les handlers, et pgx (via otelpgx) en hérite automatiquement.
	return otelhttp.NewHandler(handler, "http.server",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Pattern
			}
			return r.Method + " " + r.URL.Path
		}),
	)
}

// auditing wrappe un handler mutant : écrit une entrée audit_log à la sortie,
// avec l'identité du principal, l'IP source, le trace_id courant et le
// résultat (ok / denied / error). Audit best-effort : l'erreur d'écriture
// audit ne casse pas la réponse.
func (s *Server) auditing(action string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sr, ok := w.(*statusRecorder)
		if !ok {
			sr = &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			w = sr
		}
		h(w, r)
		p := auth.PrincipalFrom(r.Context())
		entry := model.AuditEntry{
			UserSub:   p.Sub,
			UserEmail: p.Email,
			Action:    action,
			IP:        clientIP(r),
			UserAgent: r.Header.Get("User-Agent"),
			TraceID:   traceIDFromCtx(r.Context()),
			Result:    auditResultFromStatus(sr.status),
		}
		if id := r.PathValue("id"); id != "" {
			entry.ResourceType = "ticket"
			entry.ResourceID = id
		}
		if err := s.Store.WriteAudit(r.Context(), entry); err != nil {
			slog.Warn("audit write failed", "err", err)
		}
	}
}

func auditResultFromStatus(code int) model.AuditResult {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return model.AuditDenied
	case code >= 200 && code < 400:
		return model.AuditOK
	default:
		return model.AuditError
	}
}

func traceIDFromCtx(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) listAuditHandler(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.Store.ListAudit(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, entries)
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
		defer func() { //nolint:contextcheck // metrics emission, le ctx ne sert qu'au trace_id exemplar
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "panic", rec, "path", path)
				if sr.status == http.StatusOK {
					http.Error(sr, "internal error\n", http.StatusInternalServerError)
				}
			}
			dur := time.Since(start)
			metrics.RecordHTTPCtx(r.Context(), path, sr.status, dur)
			slog.Info("request",
				"method", r.Method,
				"path", path,
				"status", sr.status,
				"duration_ms", float64(dur.Microseconds())/1000.0,
			)
		}()
		h(sr, r)
	}
}

func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	tickets, err := s.Store.ListTickets(r.Context(), "")
	if err != nil {
		http.Error(w, "list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	openCount, _ := s.Store.CountOpenTickets(r.Context())
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
	status := model.Status(r.URL.Query().Get("status"))
	tickets, err := s.Store.ListTickets(r.Context(), status)
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

	t, enqueued, err := s.Store.CreateTicketAndEnqueue(r.Context(), title, description, priority)
	if err != nil {
		http.Error(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}

	metrics.TicketsCreatedTotal.WithLabelValues(string(priority)).Inc()
	if enqueued {
		metrics.JobsEnqueuedTotal.WithLabelValues(string(model.JobKindWebhookNotify)).Inc()
	}
	if err := RefreshOpenGauge(r.Context(), s.Store); err != nil {
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

	t, err := s.Store.SetTicketStatus(r.Context(), id, model.StatusClosed)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	metrics.TicketsClosedTotal.Inc()
	if err := RefreshOpenGauge(r.Context(), s.Store); err != nil {
		slog.Warn("refresh open gauge", "err", err)
	}

	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = templates.ExecuteTemplate(w, "ticket-row", t)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func parseCreate(r *http.Request) (title, description string, priority model.Priority, err error) {
	priority = model.PriorityMedium

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
			priority = model.Priority(body.Priority)
		}
	} else {
		if err = r.ParseForm(); err != nil {
			return
		}
		title = r.FormValue("title")
		description = r.FormValue("description")
		if p := r.FormValue("priority"); p != "" {
			priority = model.Priority(p)
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
	if !model.ValidPriority(string(priority)) {
		err = errors.New("invalid priority")
		return
	}
	return
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		http.Error(w, "db unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

// workHandler simule du travail CPU pour `ms` millisecondes. Sert à exercer HPA + Kepler.
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
	_, _ = fmt.Fprintln(w, strconv.FormatUint(x&0xff, 10))
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

func RefreshOpenGauge(ctx context.Context, s *store.Store) error {
	n, err := s.CountOpenTickets(ctx)
	if err != nil {
		return err
	}
	metrics.TicketsOpen.Set(float64(n))
	return nil
}
