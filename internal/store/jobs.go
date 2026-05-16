package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Waddenn/projet-etude-app-demo/internal/model"
)

// Enqueue insère un job autonome (hors transaction ticket).
func (s *Store) Enqueue(ctx context.Context, kind model.JobKind, payload []byte) error {
	defer s.timed(ctx, "enqueue", time.Now())
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	tc := injectTraceContext(ctx)
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO jobs (kind, payload, trace_context) VALUES ($1, $2::jsonb, $3::jsonb)`,
		kind, payload, tc,
	)
	return err
}

// ClaimNextJob réserve atomiquement le prochain job prêt pour un worker.
// Retourne ErrNotFound s'il n'y en a pas.
func (s *Store) ClaimNextJob(ctx context.Context, workerID string) (*model.Job, error) {
	defer s.timed(ctx, "claim_job", time.Now())

	var j model.Job
	err := s.Pool.QueryRow(ctx, `
        UPDATE jobs
           SET status     = 'running',
               attempts   = attempts + 1,
               locked_at  = now(),
               locked_by  = $1,
               updated_at = now()
         WHERE id = (
            SELECT id FROM jobs
             WHERE status   = 'pending'
               AND run_after <= now()
             ORDER BY id
             FOR UPDATE SKIP LOCKED
             LIMIT 1
         )
        RETURNING id, kind, payload, attempts, max_attempts, trace_context
    `, workerID).Scan(&j.ID, &j.Kind, &j.Payload, &j.Attempts, &j.MaxAttempts, &j.TraceContext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *Store) CompleteJob(ctx context.Context, id int64) error {
	defer s.timed(ctx, "complete_job", time.Now())
	_, err := s.Pool.Exec(ctx,
		`UPDATE jobs SET status='done', updated_at=now() WHERE id=$1`, id)
	return err
}

// FailJob enregistre l'échec. Si attempts < maxAttempts, le job repart
// en 'pending' avec un backoff exponentiel ; sinon il passe en 'failed'.
func (s *Store) FailJob(ctx context.Context, id int64, attempts, maxAttempts int, reason string) error {
	defer s.timed(ctx, "fail_job", time.Now())
	if attempts < maxAttempts {
		backoff := time.Duration(1<<attempts) * time.Second
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		_, err := s.Pool.Exec(ctx, `
            UPDATE jobs
               SET status='pending',
                   run_after = now() + $2::interval,
                   locked_at = NULL,
                   locked_by = NULL,
                   last_error = $3,
                   updated_at = now()
             WHERE id=$1
        `, id, backoff.String(), reason)
		return err
	}
	_, err := s.Pool.Exec(ctx, `
        UPDATE jobs SET status='failed', last_error=$2, updated_at=now() WHERE id=$1
    `, id, reason)
	return err
}

func (s *Store) QueueDepth(ctx context.Context) (int64, error) {
	defer s.timed(ctx, "queue_depth", time.Now())
	var n int64
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE status='pending' AND run_after <= now()`,
	).Scan(&n)
	return n, err
}
