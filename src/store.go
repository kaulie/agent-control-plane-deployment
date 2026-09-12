package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type DeployState string

const (
	StateQueued    DeployState = "queued"
	StateRunning   DeployState = "running"
	StateSucceeded DeployState = "succeeded"
	StateFailed    DeployState = "failed"
	StateCancelled DeployState = "cancelled"
)

type ServiceContract struct {
	ServiceID  string `json:"serviceId"`
	Name       string `json:"name"`
	RuntimeDir string `json:"runtimeDir"`
	HealthURL  string `json:"healthUrl"`
	StartCmd   string `json:"startCmd"`
	StopCmd    string `json:"stopCmd"`
	RestartCmd string `json:"restartCmd"`
	// Git repo used by POST /api/deploy-notify packaging.
	GitRepoURL string `json:"gitRepoUrl,omitempty"`
	// Project-provided graceful restart endpoints (both required to enable).
	RestartNotifyURL string `json:"restartNotifyUrl,omitempty"`
	RestartPollURL   string `json:"restartPollUrl,omitempty"`
	// 0 = use server default (GRACEFUL_RESTART_MAX_WAIT_MS).
	GracefulMaxWaitMs int    `json:"gracefulRestartMaxWaitMs,omitempty"`
	CreatedAt         string `json:"createdAt"`
	UpdatedAt         string `json:"updatedAt"`
}

type DeployJob struct {
	RequestID   string      `json:"requestId"`
	ServiceID   string      `json:"serviceId"`
	Deployment  string      `json:"deployment"`
	State       DeployState `json:"state"`
	RequestedAt string      `json:"requestedAt"`
	StartedAt   string      `json:"startedAt,omitempty"`
	FinishedAt  string      `json:"finishedAt,omitempty"`
	Version     string      `json:"version,omitempty"`
	Error       string      `json:"error,omitempty"`
	Message     string      `json:"message,omitempty"`
}

type Store struct {
	db *sql.DB
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func NewStore(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
      CREATE TABLE IF NOT EXISTS services (
        service_id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        runtime_dir TEXT NOT NULL,
        health_url TEXT NOT NULL,
        start_cmd TEXT NOT NULL,
        stop_cmd TEXT NOT NULL,
        restart_cmd TEXT NOT NULL,
        watchdog_enabled INTEGER NOT NULL DEFAULT 1,
        restart_notify_url TEXT NOT NULL DEFAULT '',
        restart_poll_url TEXT NOT NULL DEFAULT '',
        graceful_max_wait_ms INTEGER NOT NULL DEFAULT 0,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS deploys (
        request_id TEXT PRIMARY KEY,
        service_id TEXT NOT NULL,
        deployment TEXT NOT NULL,
        state TEXT NOT NULL,
        requested_at TEXT NOT NULL,
        started_at TEXT,
        finished_at TEXT,
        version TEXT,
        error TEXT,
        message TEXT
      );

      CREATE INDEX IF NOT EXISTS idx_deploys_state ON deploys(state);
    `)
	if err != nil {
		return err
	}
	if err := s.ensureServiceExtraColumns(); err != nil {
		return err
	}
	return s.migratePipelines()
}

func (s *Store) ensureServiceExtraColumns() error {
	cols := map[string]string{
		"restart_notify_url":   `ALTER TABLE services ADD COLUMN restart_notify_url TEXT NOT NULL DEFAULT ''`,
		"restart_poll_url":     `ALTER TABLE services ADD COLUMN restart_poll_url TEXT NOT NULL DEFAULT ''`,
		"graceful_max_wait_ms": `ALTER TABLE services ADD COLUMN graceful_max_wait_ms INTEGER NOT NULL DEFAULT 0`,
		"git_repo_url":         `ALTER TABLE services ADD COLUMN git_repo_url TEXT NOT NULL DEFAULT ''`,
	}
	existing := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(services)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for col, ddl := range cols {
		if existing[col] {
			continue
		}
		if _, err := s.db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureServiceGracefulColumns() error {
	return s.ensureServiceExtraColumns()
}

func (s *Store) UpsertService(input ServiceContract) (ServiceContract, error) {
	existing, _ := s.GetService(input.ServiceID)
	ts := nowISO()
	row := input
	if existing != nil {
		row.CreatedAt = existing.CreatedAt
	} else if row.CreatedAt == "" {
		row.CreatedAt = ts
	}
	row.UpdatedAt = ts

	_, err := s.db.Exec(`
		INSERT INTO services (
		  service_id, name, runtime_dir, health_url,
		  start_cmd, stop_cmd, restart_cmd, watchdog_enabled,
		  restart_notify_url, restart_poll_url, graceful_max_wait_ms,
		  git_repo_url,
		  created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(service_id) DO UPDATE SET
		  name = excluded.name,
		  runtime_dir = excluded.runtime_dir,
		  health_url = excluded.health_url,
		  start_cmd = excluded.start_cmd,
		  stop_cmd = excluded.stop_cmd,
		  restart_cmd = excluded.restart_cmd,
		  watchdog_enabled = excluded.watchdog_enabled,
		  restart_notify_url = excluded.restart_notify_url,
		  restart_poll_url = excluded.restart_poll_url,
		  graceful_max_wait_ms = excluded.graceful_max_wait_ms,
		  git_repo_url = excluded.git_repo_url,
		  updated_at = excluded.updated_at`,
		row.ServiceID, row.Name, row.RuntimeDir, row.HealthURL,
		row.StartCmd, row.StopCmd, row.RestartCmd, 0,
		row.RestartNotifyURL, row.RestartPollURL, row.GracefulMaxWaitMs,
		row.GitRepoURL,
		row.CreatedAt, row.UpdatedAt,
	)
	return row, err
}

func (s *Store) GetService(serviceID string) (*ServiceContract, error) {
	row := s.db.QueryRow(`
		SELECT service_id, name, runtime_dir, health_url,
		       start_cmd, stop_cmd, restart_cmd, watchdog_enabled,
		       restart_notify_url, restart_poll_url, graceful_max_wait_ms,
		       git_repo_url,
		       created_at, updated_at
		FROM services WHERE service_id = ?`, serviceID)
	svc, err := scanService(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return svc, err
}

func (s *Store) ListServices() ([]ServiceContract, error) {
	rows, err := s.db.Query(`
		SELECT service_id, name, runtime_dir, health_url,
		       start_cmd, stop_cmd, restart_cmd, watchdog_enabled,
		       restart_notify_url, restart_poll_url, graceful_max_wait_ms,
		       git_repo_url,
		       created_at, updated_at
		FROM services ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceContract
	for rows.Next() {
		svc, err := scanServiceRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *svc)
	}
	if out == nil {
		out = []ServiceContract{}
	}
	return out, rows.Err()
}

func (s *Store) DeleteService(serviceID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM services WHERE service_id = ?`, serviceID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) CreateDeploy(requestID, serviceID, deployment, message string) (DeployJob, error) {
	requestedAt := nowISO()
	job := DeployJob{
		RequestID:   requestID,
		ServiceID:   serviceID,
		Deployment:  deployment,
		State:       StateQueued,
		RequestedAt: requestedAt,
		Message:     message,
	}
	_, err := s.db.Exec(`
		INSERT INTO deploys (
		  request_id, service_id, deployment, state, requested_at, message
		) VALUES (?, ?, ?, ?, ?, ?)`,
		job.RequestID, job.ServiceID, job.Deployment, string(job.State), job.RequestedAt, nullIfEmpty(message),
	)
	return job, err
}

func (s *Store) GetDeploy(requestID string) (*DeployJob, error) {
	row := s.db.QueryRow(`
		SELECT request_id, service_id, deployment, state, requested_at,
		       started_at, finished_at, version, error, message
		FROM deploys WHERE request_id = ?`, requestID)
	job, err := scanDeploy(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return job, err
}

func (s *Store) ListDeploys(limit int) ([]DeployJob, error) {
	rows, err := s.db.Query(`
		SELECT request_id, service_id, deployment, state, requested_at,
		       started_at, finished_at, version, error, message
		FROM deploys ORDER BY requested_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployJob
	for rows.Next() {
		job, err := scanDeployRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []DeployJob{}
	}
	return out, rows.Err()
}

func (s *Store) ListDeploysByState(state DeployState) ([]DeployJob, error) {
	rows, err := s.db.Query(`
		SELECT request_id, service_id, deployment, state, requested_at,
		       started_at, finished_at, version, error, message
		FROM deploys WHERE state = ? ORDER BY requested_at ASC`, string(state))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployJob
	for rows.Next() {
		job, err := scanDeployRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []DeployJob{}
	}
	return out, rows.Err()
}

func (s *Store) ClaimNextQueued() (*DeployJob, error) {
	row := s.db.QueryRow(`SELECT request_id FROM deploys WHERE state = 'queued' ORDER BY requested_at ASC LIMIT 1`)
	var requestID string
	if err := row.Scan(&requestID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	startedAt := nowISO()
	res, err := s.db.Exec(
		`UPDATE deploys SET state = 'running', started_at = ? WHERE request_id = ? AND state = 'queued'`,
		startedAt, requestID,
	)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	return s.GetDeploy(requestID)
}

type FinishPatch struct {
	State   DeployState
	Version string
	Error   string
	Message string
}

func (s *Store) FinishDeploy(requestID string, patch FinishPatch) (*DeployJob, error) {
	_, err := s.db.Exec(`
		UPDATE deploys SET
		  state = ?,
		  finished_at = ?,
		  version = COALESCE(?, version),
		  error = ?,
		  message = COALESCE(?, message)
		WHERE request_id = ?`,
		string(patch.State),
		nowISO(),
		nullIfEmpty(patch.Version),
		nullIfEmpty(patch.Error),
		nullIfEmpty(patch.Message),
		requestID,
	)
	if err != nil {
		return nil, err
	}
	return s.GetDeploy(requestID)
}

type scannable interface {
	Scan(dest ...any) error
}

func scanService(row scannable) (*ServiceContract, error) {
	var svc ServiceContract
	var watchdog int
	err := row.Scan(
		&svc.ServiceID, &svc.Name, &svc.RuntimeDir, &svc.HealthURL,
		&svc.StartCmd, &svc.StopCmd, &svc.RestartCmd, &watchdog,
		&svc.RestartNotifyURL, &svc.RestartPollURL, &svc.GracefulMaxWaitMs,
		&svc.GitRepoURL,
		&svc.CreatedAt, &svc.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &svc, nil
}

func scanServiceRows(rows *sql.Rows) (*ServiceContract, error) {
	return scanService(rows)
}

func scanDeploy(row scannable) (*DeployJob, error) {
	var job DeployJob
	var started, finished, version, errStr, message sql.NullString
	var state string
	err := row.Scan(
		&job.RequestID, &job.ServiceID, &job.Deployment, &state, &job.RequestedAt,
		&started, &finished, &version, &errStr, &message,
	)
	if err != nil {
		return nil, err
	}
	job.State = DeployState(state)
	job.StartedAt = started.String
	job.FinishedAt = finished.String
	job.Version = version.String
	job.Error = errStr.String
	job.Message = message.String
	return &job, nil
}

func scanDeployRows(rows *sql.Rows) (*DeployJob, error) {
	return scanDeploy(rows)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
