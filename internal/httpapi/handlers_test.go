package httpapi

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

	"github.com/Waddenn/projet-etude-app-demo/internal/auth"
	"github.com/Waddenn/projet-etude-app-demo/internal/model"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
	"github.com/Waddenn/projet-etude-app-demo/internal/testutil"
)

func setup(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	pool := testutil.NewPostgres(t)
	st := store.New(pool)
	// Auth désactivée → middleware injecte dev/operator pour tous les tests
	// hérités. Les tests d'auth dédiés instancient un Authenticator explicite.
	a, _ := auth.New(context.Background(), auth.Config{})
	return NewMux(st, a, nil), st
}

func TestHealthz(t *testing.T) {
	mux, _ := setup(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	mux, _ := setup(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAndListTicket_JSON(t *testing.T) {
	mux, _ := setup(t)

	body := strings.NewReader(`{"title":"prod down","description":"500s on /api","priority":"high"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/tickets", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created model.Ticket
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Title != "prod down" || created.Priority != model.PriorityHigh || created.Status != model.StatusOpen {
		t.Fatalf("unexpected ticket: %+v", created)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tickets", nil))
	var tickets []model.Ticket
	if err := json.Unmarshal(rec.Body.Bytes(), &tickets); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(tickets) != 1 || tickets[0].ID != created.ID {
		t.Fatalf("list mismatch: %+v", tickets)
	}
}

func TestCreateTicket_HTMXReturnsRow(t *testing.T) {
	mux, _ := setup(t)

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
	mux, st := setup(t)

	ctx := context.Background()
	ticket, err := st.CreateTicket(ctx, "to close", "", model.PriorityMedium)
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
	var got model.Ticket
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != model.StatusClosed {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestCreateTicket_ValidationRejected(t *testing.T) {
	mux, _ := setup(t)

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
	mux, _ := setup(t)

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
		`jobs_enqueued_total`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// TestCreateHighPriority_Enqueues vérifie que la priorité 'high' enfile bien
// un job webhook.notify dans la même transaction.
func TestCreateHighPriority_Enqueues(t *testing.T) {
	mux, st := setup(t)

	body := `{"title":"server on fire","priority":"high"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	n, err := st.QueueDepth(context.Background())
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 pending job, got %d", n)
	}
}

func TestCreateLowPriority_DoesNotEnqueue(t *testing.T) {
	mux, st := setup(t)

	body := `{"title":"typo","priority":"low"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	n, err := st.QueueDepth(context.Background())
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 pending jobs, got %d", n)
	}
}
