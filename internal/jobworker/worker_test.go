package jobworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Waddenn/projet-etude-app-demo/internal/model"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
	"github.com/Waddenn/projet-etude-app-demo/internal/testutil"
)

// TestWorker_ProcessesEnqueuedJob lance un worker contre Postgres, crée un
// ticket high (qui enfile un job), et attend que le job passe à 'done'.
func TestWorker_ProcessesEnqueuedJob(t *testing.T) {
	pool := testutil.NewPostgres(t)
	st := store.New(pool)

	_, _, err := st.CreateTicketAndEnqueue(context.Background(),
		"prod down", "everything is on fire", model.PriorityHigh)
	if err != nil {
		t.Fatalf("create+enqueue: %v", err)
	}

	w := New(st, pool)
	w.PollEvery = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Poll: la queue doit se vider.
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := st.QueueDepth(context.Background())
		if err != nil {
			t.Fatalf("queue depth: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue never drained, depth=%d", n)
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("worker exited with error: %v", err)
	}
}

// TestWorker_UnknownJobKindFails vérifie le chemin d'erreur : un job de kind
// inconnu doit basculer en 'failed' après épuisement des tentatives.
func TestWorker_UnknownJobKindFails(t *testing.T) {
	pool := testutil.NewPostgres(t)
	st := store.New(pool)
	ctx := context.Background()

	// On insère un job avec max_attempts=1 pour aller vite.
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (kind, payload, max_attempts) VALUES ('bogus','{}'::jsonb,1)`,
	); err != nil {
		t.Fatalf("insert bogus job: %v", err)
	}

	w := New(st, pool)
	w.PollEvery = 100 * time.Millisecond
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	go w.Run(runCtx)

	deadline := time.Now().Add(8 * time.Second)
	for {
		var status string
		err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE kind='bogus' LIMIT 1`).Scan(&status)
		if err == nil && status == "failed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reached 'failed' (last status=%q err=%v)", status, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
