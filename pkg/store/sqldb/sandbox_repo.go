package sqldb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sandrpod/sandrpod/pkg/egress"
	"github.com/sandrpod/sandrpod/pkg/sandpod"
)

type sandboxRepo struct{ db *DB }

// NewSandboxRepo returns a SQLite-backed SandboxRepository.
func NewSandboxRepo(db *DB) *sandboxRepo {
	return &sandboxRepo{db: db}
}

func (r *sandboxRepo) Add(sb *sandpod.SandboxInfo) error {
	labels, err := json.Marshal(sb.Labels)
	if err != nil {
		return fmt.Errorf("store/sqlite: sandbox Add marshal labels: %w", err)
	}
	network, err := marshalNetwork(sb)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(`
		INSERT INTO sandboxes
		  (name, id, region, provider_type, instance_type, image_id, state,
		   ip, poder_id, poder_url, container_id, proxy_url, api_url,
		   arch, os, os_version, labels, egress_policy, owner, ttl_seconds, cpu_cores, memory_mb, created_at, last_activity)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sb.Name, sb.ID, sb.Region, sb.ProviderType, sb.InstanceType, sb.ImageID,
		string(sb.State), sb.IP, sb.PoderID, sb.PoderURL, sb.ContainerID,
		sb.ProxyURL, sb.APIURL, sb.Arch, sb.OS, sb.OSVersion,
		string(labels), network, sb.Owner, sb.TTLSeconds, sb.CPUCores, sb.MemoryMB,
		sb.CreatedAt.UTC().Format(time.RFC3339Nano),
		sb.LastActivity.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("store/sqlite: sandbox Add %q: %w", sb.Name, err)
	}
	return nil
}

func (r *sandboxRepo) Get(name string) (*sandpod.SandboxInfo, bool) {
	sb, ok, err := r.getByName(name)
	if err != nil || !ok {
		return nil, false
	}
	return sb, true
}

func (r *sandboxRepo) Update(name string, fn func(*sandpod.SandboxInfo)) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("store/sqlite: sandbox Update begin: %w", err)
	}
	defer tx.Rollback()

	sb, ok, err := r.getByNameTx(tx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sandbox %q not found", name)
	}
	fn(sb)
	if err := r.upsertTx(tx, sb); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *sandboxRepo) List() []*sandpod.SandboxInfo {
	rows, err := r.db.Query(`SELECT ` + sandboxColumns + ` FROM sandboxes ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanSandboxes(rows)
}

func (r *sandboxRepo) ListByPoderID(poderID string) []*sandpod.SandboxInfo {
	rows, err := r.db.Query(
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE poder_id=? ORDER BY created_at`, poderID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanSandboxes(rows)
}

func (r *sandboxRepo) Delete(name string) error {
	res, err := r.db.Exec(`DELETE FROM sandboxes WHERE name=?`, name)
	if err != nil {
		return fmt.Errorf("store/sqlite: sandbox Delete %q: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %q not found", name)
	}
	return nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

const sandboxColumns = `name, id, region, provider_type, instance_type, image_id, state,
    ip, poder_id, poder_url, container_id, proxy_url, api_url,
    arch, os, os_version, labels, egress_policy, owner, ttl_seconds, cpu_cores, memory_mb, created_at, last_activity`

func (r *sandboxRepo) getByName(name string) (*sandpod.SandboxInfo, bool, error) {
	row := r.db.QueryRow(`SELECT `+sandboxColumns+` FROM sandboxes WHERE name=?`, name)
	return scanSandbox(row)
}

func (r *sandboxRepo) getByNameTx(tx *Tx, name string) (*sandpod.SandboxInfo, bool, error) {
	row := tx.QueryRow(`SELECT `+sandboxColumns+` FROM sandboxes WHERE name=?`, name)
	return scanSandbox(row)
}

func (r *sandboxRepo) upsertTx(tx *Tx, sb *sandpod.SandboxInfo) error {
	labels, err := json.Marshal(sb.Labels)
	if err != nil {
		return fmt.Errorf("store/sqlite: sandbox upsert marshal labels: %w", err)
	}
	network, err := marshalNetwork(sb)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		INSERT INTO sandboxes
		  (name, id, region, provider_type, instance_type, image_id, state,
		   ip, poder_id, poder_url, container_id, proxy_url, api_url,
		   arch, os, os_version, labels, egress_policy, owner, ttl_seconds, cpu_cores, memory_mb, created_at, last_activity)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
		  id=excluded.id, region=excluded.region, provider_type=excluded.provider_type,
		  instance_type=excluded.instance_type, image_id=excluded.image_id,
		  state=excluded.state, ip=excluded.ip, poder_id=excluded.poder_id,
		  poder_url=excluded.poder_url, container_id=excluded.container_id,
		  proxy_url=excluded.proxy_url, api_url=excluded.api_url,
		  arch=excluded.arch, os=excluded.os, os_version=excluded.os_version,
		  labels=excluded.labels, egress_policy=excluded.egress_policy,
		  owner=excluded.owner, ttl_seconds=excluded.ttl_seconds, last_activity=excluded.last_activity`,
		sb.Name, sb.ID, sb.Region, sb.ProviderType, sb.InstanceType, sb.ImageID,
		string(sb.State), sb.IP, sb.PoderID, sb.PoderURL, sb.ContainerID,
		sb.ProxyURL, sb.APIURL, sb.Arch, sb.OS, sb.OSVersion,
		string(labels), network, sb.Owner, sb.TTLSeconds, sb.CPUCores, sb.MemoryMB,
		sb.CreatedAt.UTC().Format(time.RFC3339Nano),
		sb.LastActivity.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// scanSandbox scans one row (from QueryRow) into a SandboxInfo.
func scanSandbox(row *sql.Row) (*sandpod.SandboxInfo, bool, error) {
	var (
		sb          sandpod.SandboxInfo
		state       string
		labelsJSON  string
		networkJSON string
		createdStr  string
		activityStr string
	)
	err := row.Scan(
		&sb.Name, &sb.ID, &sb.Region, &sb.ProviderType, &sb.InstanceType, &sb.ImageID,
		&state, &sb.IP, &sb.PoderID, &sb.PoderURL, &sb.ContainerID,
		&sb.ProxyURL, &sb.APIURL, &sb.Arch, &sb.OS, &sb.OSVersion,
		&labelsJSON, &networkJSON, &sb.Owner, &sb.TTLSeconds, &sb.CPUCores, &sb.MemoryMB, &createdStr, &activityStr,
	)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store/sqlite: sandbox scan: %w", err)
	}
	sb.State = sandpod.State(state)
	json.Unmarshal([]byte(labelsJSON), &sb.Labels)
	sb.Network = unmarshalNetwork(networkJSON)
	sb.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	sb.LastActivity, _ = time.Parse(time.RFC3339Nano, activityStr)
	return &sb, true, nil
}

// scanSandboxes scans multiple rows from a Query.
func scanSandboxes(rows *sql.Rows) []*sandpod.SandboxInfo {
	var out []*sandpod.SandboxInfo
	for rows.Next() {
		var (
			sb          sandpod.SandboxInfo
			state       string
			labelsJSON  string
			networkJSON string
			createdStr  string
			activityStr string
		)
		if err := rows.Scan(
			&sb.Name, &sb.ID, &sb.Region, &sb.ProviderType, &sb.InstanceType, &sb.ImageID,
			&state, &sb.IP, &sb.PoderID, &sb.PoderURL, &sb.ContainerID,
			&sb.ProxyURL, &sb.APIURL, &sb.Arch, &sb.OS, &sb.OSVersion,
			&labelsJSON, &networkJSON, &sb.Owner, &sb.TTLSeconds, &sb.CPUCores, &sb.MemoryMB, &createdStr, &activityStr,
		); err != nil {
			continue
		}
		sb.State = sandpod.State(state)
		json.Unmarshal([]byte(labelsJSON), &sb.Labels)
		sb.Network = unmarshalNetwork(networkJSON)
		sb.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		sb.LastActivity, _ = time.Parse(time.RFC3339Nano, activityStr)
		out = append(out, &sb)
	}
	return out
}

// marshalNetwork serialises the sandbox's egress policy for the
// egress_policy column. An absent policy stores as "" — distinct from "{}",
// which would round-trip to an empty allow-list and read as a decision.
func marshalNetwork(sb *sandpod.SandboxInfo) (string, error) {
	if sb.Network == nil {
		return "", nil
	}
	raw, err := json.Marshal(sb.Network)
	if err != nil {
		return "", fmt.Errorf("store/sqlite: sandbox marshal egress policy: %w", err)
	}
	return string(raw), nil
}

// unmarshalNetwork is the read side; an unparsable column is treated as "no
// policy" rather than failing the whole record, so one bad row cannot make a
// sandbox invisible.
func unmarshalNetwork(raw string) *egress.Policy {
	if raw == "" {
		return nil
	}
	var p egress.Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil
	}
	return &p
}
