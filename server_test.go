package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupTestServer(t *testing.T) (http.Handler, *Store, func()) {
	t.Helper()
	ctx := context.Background()

	pgC, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("app"),
		tcpostgres.WithUsername("app"),
		tcpostgres.WithPassword("app"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("postgres container: %v", err)
	}

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse cfg: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}

	if err := migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := &Store{pool: pool}
	mux := newMux(store)

	cleanup := func() {
		pool.Close()
		_ = pgC.Terminate(context.Background())
	}
	return mux, store, cleanup
}

func TestHealthz(t *testing.T) {
	mux, _, cleanup := setupTestServer(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	mux, _, cleanup := setupTestServer(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAndListTicket_JSON(t *testing.T) {
	mux, _, cleanup := setupTestServer(t)
	defer cleanup()

	body := strings.NewReader(`{"title":"prod down","description":"500s on /api","priority":"high"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/tickets", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created Ticket
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Title != "prod down" || created.Priority != PriorityHigh || created.Status != StatusOpen {
		t.Fatalf("unexpected ticket: %+v", created)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tickets", nil))
	var tickets []Ticket
	if err := json.Unmarshal(rec.Body.Bytes(), &tickets); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(tickets) != 1 || tickets[0].ID != created.ID {
		t.Fatalf("list mismatch: %+v", tickets)
	}
}

func TestCreateTicket_HTMXReturnsRow(t *testing.T) {
	mux, _, cleanup := setupTestServer(t)
	defer cleanup()

	form := url.Values{}
	form.Set("title", "imprimante HS")
	form.Set("priority", "low")

	req := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<tr") || !strings.Contains(body, "imprimante HS") {
		t.Fatalf("expected HTML row, got: %s", body)
	}
}

func TestCloseTicket(t *testing.T) {
	mux, store, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()
	ticket, err := store.Create(ctx, "to close", "", PriorityMedium)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tickets/"+strconv.FormatInt(ticket.ID, 10)+"/close", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got Ticket
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != StatusClosed {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestCreateTicket_ValidationRejected(t *testing.T) {
	mux, _, cleanup := setupTestServer(t)
	defer cleanup()

	cases := []struct {
		name string
		body string
	}{
		{"empty title", `{"title":"","priority":"low"}`},
		{"bad priority", `{"title":"x","priority":"urgent"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				body, _ := io.ReadAll(rec.Body)
				t.Fatalf("expected 400, got %d body=%s", rec.Code, body)
			}
		})
	}
}

func TestMetricsExposed(t *testing.T) {
	mux, _, cleanup := setupTestServer(t)
	defer cleanup()

	body := `{"title":"obs","priority":"high"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`tickets_created_total{priority="high"}`,
		`tickets_open`,
		`db_query_duration_seconds`,
		`http_requests_total`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

