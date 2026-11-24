package sqldb_test

import (
	"errors"
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

// The outbox claim is a compare-and-swap on the stored text, and Postgres keeps
// that text exactly as written. A row inserted by something other than this
// process — an import, an operator's psql, an older build — spells the same
// instant differently ("…40.813130Z" from to_char vs "…40.81313Z" from Go), and
// the timestamp round-trip claim then never matches. The job sits PENDING, the
// resumer says nothing, and the delivery it exists to save is lost. The raw claim
// compares the spelling the row actually carries.
func TestPostgres_ClaimJobRaw_AcceptsAForeignTimestampSpelling(t *testing.T) {
	db := openPG(t)
	repo := sqldb.NewJobRepo(db)

	stored := "2026-11-20T07:46:40.813130Z"
	if _, err := db.Exec(
		`INSERT INTO jobs (id, type, status, sandbox_name, request, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"foreign-job", "CREATE_SANDBOX", "PENDING", "sb-foreign", "{}", stored, stored,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	job, ok := repo.GetJob("foreign-job")
	if !ok {
		t.Fatal("the row did not read back")
	}
	if job.UpdatedAtRaw != stored {
		t.Fatalf("UpdatedAtRaw = %q, want the stored spelling %q", job.UpdatedAtRaw, stored)
	}

	// Pinned: the round-trip claim cannot match, which is why the outbox skipped
	// exactly the rows it exists for.
	if claimed, err := repo.ClaimJob("foreign-job", job.UpdatedAt, time.Now()); err != nil {
		t.Fatalf("ClaimJob: %v", err)
	} else if claimed {
		t.Fatal("the round-trip claim matched; this test no longer proves anything")
	}

	raw, ok := interface{}(repo).(sandpod.RawTimeClaimer)
	if !ok {
		t.Fatal("the SQL store must implement RawTimeClaimer")
	}
	claimed, err := raw.ClaimJobRaw("foreign-job", job.UpdatedAtRaw, time.Now())
	if err != nil {
		t.Fatalf("ClaimJobRaw: %v", err)
	}
	if !claimed {
		t.Fatal("the row's own spelling was refused — the outbox would skip it forever")
	}
	if claimed, _ := raw.ClaimJobRaw("foreign-job", job.UpdatedAtRaw, time.Now()); claimed {
		t.Fatal("a second claim on the same timestamp succeeded")
	}
}

// The race the guard exists for: two instances revoke the last two keys at the
// same moment. Without FOR UPDATE both transactions count two rows, both
// delete, and the table ends empty; with it the second waits, re-reads, and
// refuses. Exactly one delete must succeed.
func TestPostgres_TokenDeleteKeepsOneKey(t *testing.T) {
	db := openPG(t)
	repo := sqldb.NewTokenRepo(db)
	for _, tok := range []*sandpod.APIToken{
		{Name: "a", Prefix: "e2b_aaaa", Hash: "h-a", Role: "user", CreatedAt: time.Now()},
		{Name: "b", Prefix: "e2b_bbbb", Hash: "h-b", Role: "user", CreatedAt: time.Now()},
	} {
		if err := repo.Create(tok); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, prefix := range []string{"e2b_aaaa", "e2b_bbbb"} {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			<-start
			_, err := repo.DeleteByPrefixIfNotLast(p)
			errs <- err
		}(prefix)
	}
	close(start)
	wg.Wait()
	close(errs)

	refusals := 0
	for err := range errs {
		if errors.Is(err, sandpod.ErrLastToken) {
			refusals++
		} else if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if refusals != 1 {
		t.Fatalf("refusals = %d, want exactly 1 (both deletes succeeded or both refused)", refusals)
	}
	if list, _ := repo.List(); len(list) != 1 {
		t.Fatalf("tokens left = %d, want 1", len(list))
	}
}
