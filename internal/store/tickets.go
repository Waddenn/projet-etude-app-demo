package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Waddenn/projet-etude-app-demo/internal/model"
)

// injectTraceContext sérialise le span courant dans une jsonb. Vide si pas de span.
func injectTraceContext(ctx context.Context) []byte {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	b, _ := json.Marshal(carrier)
	return b
}

func (s *Store) CreateTicket(ctx context.Context, title, description string, priority model.Priority) (*model.Ticket, error) {
	defer s.timed(ctx, "create", time.Now())

	var t model.Ticket
	err := s.Pool.QueryRow(ctx, `
        INSERT INTO tickets (title, description, priority)
        VALUES ($1, $2, $3)
        RETURNING id, title, description, priority, status, created_at, updated_at
    `, title, description, priority).Scan(
		&t.ID, &t.Title, &t.Description, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTicketAndEnqueue insère le ticket et, si la priorité est "high",
// enfile un job webhook.notify dans la même transaction.
func (s *Store) CreateTicketAndEnqueue(ctx context.Context, title, description string, priority model.Priority) (*model.Ticket, bool, error) {
	defer s.timed(ctx, "create_with_job", time.Now())

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	var t model.Ticket
	if err := tx.QueryRow(ctx, `
        INSERT INTO tickets (title, description, priority)
        VALUES ($1, $2, $3)
        RETURNING id, title, description, priority, status, created_at, updated_at
    `, title, description, priority).Scan(
		&t.ID, &t.Title, &t.Description, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, false, err
	}

	enqueued := false
	if priority == model.PriorityHigh {
		payload := []byte(`{"ticket_id":` + strconv.FormatInt(t.ID, 10) + `}`)
		tc := injectTraceContext(ctx)
		if _, err := tx.Exec(ctx,
			`INSERT INTO jobs (kind, payload, trace_context) VALUES ($1, $2::jsonb, $3::jsonb)`,
			model.JobKindWebhookNotify, payload, tc,
		); err != nil {
			return nil, false, err
		}
		enqueued = true
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &t, enqueued, nil
}

func (s *Store) ListTickets(ctx context.Context, status model.Status) ([]model.Ticket, error) {
	defer s.timed(ctx, "list", time.Now())

	q := `SELECT id, title, description, priority, status, created_at, updated_at FROM tickets`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 200`

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.Ticket, 0, 32)
	for rows.Next() {
		var t model.Ticket
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTicket(ctx context.Context, id int64) (*model.Ticket, error) {
	defer s.timed(ctx, "get", time.Now())

	var t model.Ticket
	err := s.Pool.QueryRow(ctx, `
        SELECT id, title, description, priority, status, created_at, updated_at
        FROM tickets WHERE id = $1
    `, id).Scan(&t.ID, &t.Title, &t.Description, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) SetTicketStatus(ctx context.Context, id int64, status model.Status) (*model.Ticket, error) {
	defer s.timed(ctx, "set_status", time.Now())

	var t model.Ticket
	err := s.Pool.QueryRow(ctx, `
        UPDATE tickets SET status = $1, updated_at = now() WHERE id = $2
        RETURNING id, title, description, priority, status, created_at, updated_at
    `, status, id).Scan(&t.ID, &t.Title, &t.Description, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) CountOpenTickets(ctx context.Context) (int64, error) {
	defer s.timed(ctx, "count_open", time.Now())

	var n int64
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE status = 'open'`).Scan(&n)
	return n, err
}

