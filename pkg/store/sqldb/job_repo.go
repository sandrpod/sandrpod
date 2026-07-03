package sqldb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sandrpod/sandrpod/pkg/sandpod"
)

type jobRepo struct{ db *DB }

// NewJobRepo returns a SQLite-backed JobRepository.
func NewJobRepo(db *DB) *jobRepo {
	return &jobRepo{db: db}
}

func (r *jobRepo) AddJob(job *sandpod.Job) error {
	traceJSON, _ := json.Marshal(job.TraceContext)
	var resultJSON *string
	if job.Result != nil {
		b, _ := json.Marshal(job.Result)
		s := string(b)
		resultJSON = &s
	}
	_, err := r.db.Exec(`
		INSERT INTO jobs
		  (id, type, status, sandbox_name, sandbox_id, region, provider_type,
		   poder_id, poder_url, vm_id, instance_type, image_id, command, language,
		   result, error_message, trace_context, owner, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ID, string(job.Type), string(job.Status),
		job.SandboxName, job.SandboxID, job.Region, job.ProviderType,
		job.PoderID, job.PoderURL, job.VmID, job.InstanceType, job.ImageID,
		job.Command, job.Language,
		resultJSON, job.ErrorMessage, string(traceJSON), job.Owner,
		job.CreatedAt.UTC().Format(time.RFC3339Nano),
		job.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("store/sqlite: job AddJob %q: %w", job.ID, err)
	}
	return nil
}

func (r *jobRepo) GetJob(id string) (*sandpod.Job, bool) {
	j, ok, err := r.getByID(id)
	if err != nil || !ok {
		return nil, false
	}
	return j, true
}

func (r *jobRepo) UpdateJob(id string, fn func(*sandpod.Job)) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("store/sqlite: job UpdateJob begin: %w", err)
	}
	defer tx.Rollback()

	j, ok, err := r.getByIDTx(tx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("job %q not found", id)
	}

	fn(j)
	j.UpdatedAt = time.Now()

	if err := r.upsertTx(tx, j); err != nil {
		return err
	}
	return tx.Commit()
}

// PollJobs atomically resets timed-out IN_PROGRESS jobs to PENDING, then
// claims up to limit PENDING jobs (marks them IN_PROGRESS) and returns them.
//
// Concurrency: on SQLite the single writer (SetMaxOpenConns(1)) serialises this
// whole transaction; on Postgres the claim SELECT adds FOR UPDATE SKIP LOCKED
// (dialect ClaimLock) so concurrent pollers claim disjoint job sets.
func (r *jobRepo) PollJobs(jobTimeout time.Duration, limit int) ([]*sandpod.Job, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: PollJobs begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	cutoff := now.Add(-jobTimeout).Format(time.RFC3339Nano)
	nowStr := now.Format(time.RFC3339Nano)

	// Step 1: reset timed-out IN_PROGRESS jobs back to PENDING.
	// Use a Go-formatted timestamp for updated_at so the format is consistent
	// with the RFC3339Nano strings we compare in the WHERE clause.
	if _, err := tx.Exec(`
		UPDATE jobs SET status='PENDING', updated_at=?
		WHERE status='IN_PROGRESS' AND updated_at < ?`, nowStr, cutoff,
	); err != nil {
		return nil, fmt.Errorf("store/sqlite: PollJobs reset: %w", err)
	}

	// Step 2: select ids of PENDING jobs to claim. On Postgres the dialect adds
	// FOR UPDATE SKIP LOCKED so concurrent pollers claim disjoint sets; on SQLite
	// the single writer already serialises this (ClaimLock is empty).
	rows, err := tx.Query(
		`SELECT id FROM jobs WHERE status='PENDING' ORDER BY created_at LIMIT ?`+r.db.d.ClaimLock(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: PollJobs select: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()

	if len(ids) == 0 {
		tx.Commit()
		return nil, nil
	}

	// Step 3: mark claimed jobs as IN_PROGRESS.
	for _, id := range ids {
		if _, err := tx.Exec(
			`UPDATE jobs SET status='IN_PROGRESS', updated_at=? WHERE id=?`, nowStr, id,
		); err != nil {
			return nil, fmt.Errorf("store/sqlite: PollJobs mark %q: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store/sqlite: PollJobs commit: %w", err)
	}

	// Step 4: read full rows outside the transaction (safe: single-writer connection).
	result := make([]*sandpod.Job, 0, len(ids))
	for _, id := range ids {
		j, ok, err := r.getByID(id)
		if err != nil || !ok {
			continue
		}
		result = append(result, j)
	}
	return result, nil
}

func (r *jobRepo) ListJobs() []*sandpod.Job {
	rows, err := r.db.Query(`SELECT ` + jobColumns + ` FROM jobs ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanJobs(rows)
}

// ─── helpers ────────────────────────────────────────────────────────────────

const jobColumns = `id, type, status, sandbox_name, sandbox_id, region, provider_type,
    poder_id, poder_url, vm_id, instance_type, image_id, command, language,
    result, error_message, trace_context, owner, created_at, updated_at`

func (r *jobRepo) getByID(id string) (*sandpod.Job, bool, error) {
	row := r.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id=?`, id)
	return scanJob(row)
}

func (r *jobRepo) getByIDTx(tx *Tx, id string) (*sandpod.Job, bool, error) {
	row := tx.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id=?`, id)
	return scanJob(row)
}

func (r *jobRepo) upsertTx(tx *Tx, j *sandpod.Job) error {
	traceJSON, _ := json.Marshal(j.TraceContext)
	var resultJSON *string
	if j.Result != nil {
		b, _ := json.Marshal(j.Result)
		s := string(b)
		resultJSON = &s
	}
	_, err := tx.Exec(`
		INSERT INTO jobs
		  (id, type, status, sandbox_name, sandbox_id, region, provider_type,
		   poder_id, poder_url, vm_id, instance_type, image_id, command, language,
		   result, error_message, trace_context, owner, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  type=excluded.type, status=excluded.status,
		  sandbox_name=excluded.sandbox_name, sandbox_id=excluded.sandbox_id,
		  region=excluded.region, provider_type=excluded.provider_type,
		  poder_id=excluded.poder_id, poder_url=excluded.poder_url,
		  vm_id=excluded.vm_id, instance_type=excluded.instance_type,
		  image_id=excluded.image_id, command=excluded.command,
		  language=excluded.language, result=excluded.result,
		  error_message=excluded.error_message, trace_context=excluded.trace_context,
		  owner=excluded.owner, updated_at=excluded.updated_at`,
		j.ID, string(j.Type), string(j.Status),
		j.SandboxName, j.SandboxID, j.Region, j.ProviderType,
		j.PoderID, j.PoderURL, j.VmID, j.InstanceType, j.ImageID,
		j.Command, j.Language,
		resultJSON, j.ErrorMessage, string(traceJSON), j.Owner,
		j.CreatedAt.UTC().Format(time.RFC3339Nano),
		j.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func scanJob(row *sql.Row) (*sandpod.Job, bool, error) {
	var (
		j          sandpod.Job
		jobType    string
		status     string
		resultJSON sql.NullString
		traceJSON  string
		createdStr string
		updatedStr string
	)
	err := row.Scan(
		&j.ID, &jobType, &status,
		&j.SandboxName, &j.SandboxID, &j.Region, &j.ProviderType,
		&j.PoderID, &j.PoderURL, &j.VmID, &j.InstanceType, &j.ImageID,
		&j.Command, &j.Language,
		&resultJSON, &j.ErrorMessage, &traceJSON,
		&j.Owner, &createdStr, &updatedStr,
	)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store/sqlite: job scan: %w", err)
	}
	j.Type = sandpod.JobType(jobType)
	j.Status = sandpod.JobStatus(status)
	if resultJSON.Valid {
		j.Result = &sandpod.JobResult{}
		json.Unmarshal([]byte(resultJSON.String), j.Result)
	}
	json.Unmarshal([]byte(traceJSON), &j.TraceContext)
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	j.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	return &j, true, nil
}

func scanJobs(rows *sql.Rows) []*sandpod.Job {
	var out []*sandpod.Job
	for rows.Next() {
		var (
			j          sandpod.Job
			jobType    string
			status     string
			resultJSON sql.NullString
			traceJSON  string
			createdStr string
			updatedStr string
		)
		if err := rows.Scan(
			&j.ID, &jobType, &status,
			&j.SandboxName, &j.SandboxID, &j.Region, &j.ProviderType,
			&j.PoderID, &j.PoderURL, &j.VmID, &j.InstanceType, &j.ImageID,
			&j.Command, &j.Language,
			&resultJSON, &j.ErrorMessage, &traceJSON,
			&j.Owner, &createdStr, &updatedStr,
		); err != nil {
			continue
		}
		j.Type = sandpod.JobType(jobType)
		j.Status = sandpod.JobStatus(status)
		if resultJSON.Valid {
			j.Result = &sandpod.JobResult{}
			json.Unmarshal([]byte(resultJSON.String), j.Result)
		}
		json.Unmarshal([]byte(traceJSON), &j.TraceContext)
		j.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		j.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
		out = append(out, &j)
	}
	return out
}
