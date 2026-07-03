package sqldb_test

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store/sqldb"
)

// openPG opens the Postgres under test (TEST_POSTGRES_DSN) and truncates all
// tables so each test starts clean. Skips when the env var is unset, so the
// suite still runs (SQLite-only) without a Postgres available.
func openPG(t *testing.T) *sqldb.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run Postgres-backed tests")
	}
	db, err := sqldb.Open(dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if _, err := db.Exec(`TRUNCATE sandboxes, poders, jobs, api_tokens`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestPostgres_Repos exercises the shared repositories against a real Postgres:
// ON CONFLICT upserts, the poder scoring query (SelectBest), and token CRUD —
// proving the dialect layer (?→$N rebind, TEXT/RFC3339 timestamps, DDL) round-trips.
func TestPostgres_Repos(t *testing.T) {
	db := openPG(t)

	sb := sqldb.NewSandboxRepo(db)
	s := &sandpod.SandboxInfo{
		Name: "pg", ID: "i", Region: "r", State: sandpod.StatePending,
		Labels: map[string]string{"a": "b"}, CreatedAt: time.Now(), LastActivity: time.Now(),
	}
	if err := sb.Add(s); err != nil {
		t.Fatalf("sandbox add: %v", err)
	}
	if err := sb.Update("pg", func(x *sandpod.SandboxInfo) { x.State = sandpod.StateRunning }); err != nil {
		t.Fatalf("sandbox update: %v", err)
	}
	if g, ok := sb.Get("pg"); !ok || g.State != sandpod.StateRunning || g.Labels["a"] != "b" {
		t.Fatalf("sandbox roundtrip: %+v ok=%v", g, ok)
	}

	pr := sqldb.NewPoderRepo(db)
	if _, err := pr.Register(newPoderReq("pod1", "r", "local")); err != nil {
		t.Fatalf("poder register: %v", err)
	}
	if _, err := pr.SelectBest("r", "local"); err != nil {
		t.Fatalf("poder SelectBest: %v", err)
	}

	tr := sqldb.NewTokenRepo(db)
	if err := tr.Create(&sandpod.APIToken{Name: "n", Prefix: "e2b_x", Hash: "h", Role: "user", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("token create: %v", err)
	}
	if l, _ := tr.List(); len(l) != 1 {
		t.Fatalf("token list: got %d, want 1", len(l))
	}
}

// TestPostgres_JobConcurrentClaim is the payoff of moving to Postgres: N pending
// jobs, M concurrent pollers, and FOR UPDATE SKIP LOCKED must hand each job to
// exactly one poller (no double-claim) — the concurrent claiming a single-writer
// SQLite can't provide.
func TestPostgres_JobConcurrentClaim(t *testing.T) {
	db := openPG(t)
	repo := sqldb.NewJobRepo(db)

	const nJobs = 60
	for i := 0; i < nJobs; i++ {
		if err := repo.AddJob(newJob(fmt.Sprintf("job-%03d", i))); err != nil {
			t.Fatalf("add job: %v", err)
		}
	}

	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				jobs, err := repo.PollJobs(time.Hour, 5)
				if err != nil {
					t.Errorf("poll: %v", err)
					return
				}
				if len(jobs) == 0 {
					return
				}
				mu.Lock()
				for _, j := range jobs {
					seen[j.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != nJobs {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), nJobs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %s double-claimed (%d times)", id, n)
		}
	}
}
