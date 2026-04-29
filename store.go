package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
)

func ValidPriority(p string) bool {
	switch Priority(p) {
	case PriorityLow, PriorityMedium, PriorityHigh:
		return true
	}
	return false
}

type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

type Ticket struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Priority    Priority  `json:"priority"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

var ErrNotFound = errors.New("ticket not found")

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS tickets (
        id          BIGSERIAL PRIMARY KEY,
        title       TEXT      NOT NULL CHECK (length(title) > 0 AND length(title) <= 200),
        description TEXT      NOT NULL DEFAULT '',
        priority    TEXT      NOT NULL DEFAULT 'medium'
                              CHECK (priority IN ('low','medium','high')),
        status      TEXT      NOT NULL DEFAULT 'open'
                              CHECK (status IN ('open','closed')),
        created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE INDEX IF NOT EXISTS tickets_status_created_at_idx
        ON tickets (status, created_at DESC)`,
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	for _, stmt := range migrations {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Create(ctx context.Context, title, description string, priority Priority) (*Ticket, error) {
	t0 := time.Now()
	defer func() { observeQuery("create", t0) }()

	var t Ticket
	err := s.pool.QueryRow(ctx, `
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

func (s *Store) List(ctx context.Context, status Status) ([]Ticket, error) {
	t0 := time.Now()
	defer func() { observeQuery("list", t0) }()

	q := `SELECT id, title, description, priority, status, created_at, updated_at FROM tickets`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 200`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Ticket, 0, 32)
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, id int64) (*Ticket, error) {
	t0 := time.Now()
	defer func() { observeQuery("get", t0) }()

	var t Ticket
	err := s.pool.QueryRow(ctx, `
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

func (s *Store) SetStatus(ctx context.Context, id int64, status Status) (*Ticket, error) {
	t0 := time.Now()
	defer func() { observeQuery("set_status", t0) }()

	var t Ticket
	err := s.pool.QueryRow(ctx, `
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

func (s *Store) CountOpen(ctx context.Context) (int64, error) {
	t0 := time.Now()
	defer func() { observeQuery("count_open", t0) }()

	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE status = 'open'`).Scan(&n)
	return n, err
}

func (s *Store) Ping(ctx context.Context) error {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(c)
}

func refreshOpenGauge(ctx context.Context, store *Store) error {
	n, err := store.CountOpen(ctx)
	if err != nil {
		return fmt.Errorf("count open: %w", err)
	}
	ticketsOpen.Set(float64(n))
	return nil
}
