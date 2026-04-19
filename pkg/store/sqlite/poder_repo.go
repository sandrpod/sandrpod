package sqlite

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/sandrpod/sandrpod/pkg/sandpod"
)

type poderRepo struct{ db *sql.DB }

// NewPoderRepo returns a SQLite-backed PoderRepository.
func NewPoderRepo(db *sql.DB) *poderRepo {
	return &poderRepo{db: db}
}

func (r *poderRepo) Register(req *sandpod.RegisterPoderRequest) (*sandpod.PoderInfo, error) {
	now := time.Now().UTC()

	// Upsert: update resources on re-register, preserve created_at if already exists.
	_, err := r.db.Exec(`
		INSERT INTO poders
		  (id, name, url, region, provider_type, state,
		   cpu_cores, memory_bytes, max_containers, arch, os, os_version, kernel_version,
		   usage_containers, usage_cpu, usage_memory,
		   last_heartbeat, created_at)
		VALUES (?,?,?,?,?,'ONLINE',?,?,?,?,?,?,?,0,0,0,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  name=excluded.name, url=excluded.url, region=excluded.region,
		  provider_type=excluded.provider_type, state='ONLINE',
		  cpu_cores=excluded.cpu_cores, memory_bytes=excluded.memory_bytes,
		  max_containers=excluded.max_containers, arch=excluded.arch,
		  os=excluded.os, os_version=excluded.os_version,
		  kernel_version=excluded.kernel_version,
		  last_heartbeat=excluded.last_heartbeat`,
		req.ID, req.Name, req.URL, req.Region, req.ProviderType,
		req.Resources.CPUCores, req.Resources.MemoryBytes, req.Resources.MaxContainers,
		req.Resources.Arch, req.Resources.OS, req.Resources.OSVersion, req.Resources.KernelVersion,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: poder Register %q: %w", req.ID, err)
	}

	p, ok, err := r.getByID(req.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("store/sqlite: poder %q not found after register", req.ID)
	}
	return p, nil
}

func (r *poderRepo) Heartbeat(id string, usage *sandpod.HeartbeatRequest) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := r.db.Exec(`
		UPDATE poders SET
		  usage_containers=?, usage_cpu=?, usage_memory=?,
		  last_heartbeat=?
		WHERE id=?`,
		usage.Containers, usage.CPUUsage, usage.MemoryUsage, now, id,
	)
	if err != nil {
		return fmt.Errorf("store/sqlite: poder Heartbeat %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("poder %q not found", id)
	}
	return nil
}

func (r *poderRepo) Get(id string) (*sandpod.PoderInfo, bool) {
	p, ok, err := r.getByID(id)
	if err != nil || !ok {
		return nil, false
	}
	return p, true
}

func (r *poderRepo) List() []*sandpod.PoderInfo {
	rows, err := r.db.Query(`SELECT ` + poderColumns + ` FROM poders ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanPoders(rows)
}

// SelectBest returns the least-loaded ONLINE Poder matching the filters.
// The scoring formula mirrors the in-memory implementation:
//
//	score = (containers/max_containers)*0.6 + cpu*0.2 + memory*0.2
func (r *poderRepo) SelectBest(region, providerType string) (*sandpod.PoderInfo, error) {
	var id string
	err := r.db.QueryRow(`
		SELECT id FROM poders
		WHERE state='ONLINE'
		  AND usage_containers < max_containers
		  AND (? = '' OR region = ?)
		  AND (? = '' OR provider_type = ?)
		ORDER BY
		  (CAST(usage_containers AS REAL) / NULLIF(max_containers, 0)) * 0.6
		  + usage_cpu    * 0.2
		  + usage_memory * 0.2 ASC
		LIMIT 1`,
		region, region, providerType, providerType,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no available poder found (region=%q provider=%q)", region, providerType)
	}
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: poder SelectBest: %w", err)
	}

	p, ok, err := r.getByID(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("store/sqlite: poder %q disappeared after SelectBest", id)
	}
	return p, nil
}

func (r *poderRepo) UpdateUsage(id string, fn func(*sandpod.PoderUsage)) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("store/sqlite: poder UpdateUsage begin: %w", err)
	}
	defer tx.Rollback()

	p, ok, err := r.getByIDTx(tx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("poder %q not found", id)
	}

	fn(&p.Usage)

	_, err = tx.Exec(`
		UPDATE poders SET usage_containers=?, usage_cpu=?, usage_memory=? WHERE id=?`,
		p.Usage.Containers, p.Usage.CPUUsage, p.Usage.MemoryUsage, id,
	)
	if err != nil {
		return fmt.Errorf("store/sqlite: poder UpdateUsage exec: %w", err)
	}
	return tx.Commit()
}

func (r *poderRepo) SetOffline(id string) {
	r.db.Exec(`UPDATE poders SET state='OFFLINE' WHERE id=?`, id)
}

// ─── helpers ────────────────────────────────────────────────────────────────

const poderColumns = `id, name, url, region, provider_type, state,
    cpu_cores, memory_bytes, max_containers, arch, os, os_version, kernel_version,
    usage_containers, usage_cpu, usage_memory,
    last_heartbeat, created_at`

func (r *poderRepo) getByID(id string) (*sandpod.PoderInfo, bool, error) {
	row := r.db.QueryRow(`SELECT `+poderColumns+` FROM poders WHERE id=?`, id)
	return scanPoder(row)
}

func (r *poderRepo) getByIDTx(tx *sql.Tx, id string) (*sandpod.PoderInfo, bool, error) {
	row := tx.QueryRow(`SELECT `+poderColumns+` FROM poders WHERE id=?`, id)
	return scanPoder(row)
}

func scanPoder(row *sql.Row) (*sandpod.PoderInfo, bool, error) {
	var (
		p             sandpod.PoderInfo
		state         string
		heartbeatStr  string
		createdStr    string
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.URL, &p.Region, &p.ProviderType, &state,
		&p.Resources.CPUCores, &p.Resources.MemoryBytes, &p.Resources.MaxContainers,
		&p.Resources.Arch, &p.Resources.OS, &p.Resources.OSVersion, &p.Resources.KernelVersion,
		&p.Usage.Containers, &p.Usage.CPUUsage, &p.Usage.MemoryUsage,
		&heartbeatStr, &createdStr,
	)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store/sqlite: poder scan: %w", err)
	}
	p.State = sandpod.PoderState(state)
	p.LastHeartbeat, _ = time.Parse(time.RFC3339Nano, heartbeatStr)
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &p, true, nil
}

func scanPoders(rows *sql.Rows) []*sandpod.PoderInfo {
	var out []*sandpod.PoderInfo
	for rows.Next() {
		var (
			p             sandpod.PoderInfo
			state         string
			heartbeatStr  string
			createdStr    string
		)
		if err := rows.Scan(
			&p.ID, &p.Name, &p.URL, &p.Region, &p.ProviderType, &state,
			&p.Resources.CPUCores, &p.Resources.MemoryBytes, &p.Resources.MaxContainers,
			&p.Resources.Arch, &p.Resources.OS, &p.Resources.OSVersion, &p.Resources.KernelVersion,
			&p.Usage.Containers, &p.Usage.CPUUsage, &p.Usage.MemoryUsage,
			&heartbeatStr, &createdStr,
		); err != nil {
			continue
		}
		p.State = sandpod.PoderState(state)
		p.LastHeartbeat, _ = time.Parse(time.RFC3339Nano, heartbeatStr)
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		out = append(out, &p)
	}
	return out
}
