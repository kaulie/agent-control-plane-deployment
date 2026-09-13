package main

import (
	"database/sql"
	"fmt"
)

type PipelineState string

const (
	PipelineQueued    PipelineState = "queued"
	PipelinePackaging PipelineState = "packaging"
	PipelineDeploying PipelineState = "deploying"
	PipelineSucceeded PipelineState = "succeeded"
	PipelineFailed    PipelineState = "failed"
)

type PipelineJob struct {
	RequestID       string        `json:"requestId"`
	ServiceID       string        `json:"serviceId"`
	Ref             string        `json:"ref"`
	State           PipelineState `json:"state"`
	Deployment      string        `json:"deployment,omitempty"`
	DeployRequestID string        `json:"deployRequestId,omitempty"`
	Version         string        `json:"version,omitempty"`
	Error           string        `json:"error,omitempty"`
	Message         string        `json:"message,omitempty"`
	RequestedAt     string        `json:"requestedAt"`
	StartedAt       string        `json:"startedAt,omitempty"`
	FinishedAt      string        `json:"finishedAt,omitempty"`
}

func (s *Store) migratePipelines() error {
	_, err := s.db.Exec(`
      CREATE TABLE IF NOT EXISTS pipelines (
        request_id TEXT PRIMARY KEY,
        service_id TEXT NOT NULL,
        ref TEXT NOT NULL,
        state TEXT NOT NULL,
        deployment TEXT,
        deploy_request_id TEXT,
        version TEXT,
        error TEXT,
        message TEXT,
        requested_at TEXT NOT NULL,
        started_at TEXT,
        finished_at TEXT
      );
      CREATE INDEX IF NOT EXISTS idx_pipelines_state ON pipelines(state);
    `)
	return err
}

func (s *Store) CreatePipeline(requestID, serviceID, ref, message string) (PipelineJob, error) {
	requestedAt := nowISO()
	job := PipelineJob{
		RequestID:   requestID,
		ServiceID:   serviceID,
		Ref:         ref,
		State:       PipelineQueued,
		RequestedAt: requestedAt,
		Message:     message,
	}
	_, err := s.db.Exec(`
		INSERT INTO pipelines (
		  request_id, service_id, ref, state, message, requested_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		job.RequestID, job.ServiceID, job.Ref, string(job.State),
		nullIfEmpty(message), job.RequestedAt,
	)
	return job, err
}

func (s *Store) GetPipeline(requestID string) (*PipelineJob, error) {
	row := s.db.QueryRow(`
		SELECT request_id, service_id, ref, state, deployment, deploy_request_id,
		       version, error, message, requested_at, started_at, finished_at
		FROM pipelines WHERE request_id = ?`, requestID)
	job, err := scanPipeline(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return job, err
}

func (s *Store) ListPipelines(limit int) ([]PipelineJob, error) {
	rows, err := s.db.Query(`
		SELECT request_id, service_id, ref, state, deployment, deploy_request_id,
		       version, error, message, requested_at, started_at, finished_at
		FROM pipelines ORDER BY requested_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipelineJob
	for rows.Next() {
		job, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []PipelineJob{}
	}
	return out, rows.Err()
}

func (s *Store) ClaimNextPipeline() (*PipelineJob, error) {
	row := s.db.QueryRow(`SELECT request_id FROM pipelines WHERE state = 'queued' ORDER BY requested_at ASC LIMIT 1`)
	var requestID string
	if err := row.Scan(&requestID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	startedAt := nowISO()
	res, err := s.db.Exec(
		`UPDATE pipelines SET state = 'packaging', started_at = ?, message = ? WHERE request_id = ? AND state = 'queued'`,
		startedAt, "packaging from git", requestID,
	)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	return s.GetPipeline(requestID)
}

func (s *Store) UpdatePipeline(requestID string, patch PipelineJob) error {
	_, err := s.db.Exec(`
		UPDATE pipelines SET
		  state = ?,
		  deployment = COALESCE(?, deployment),
		  deploy_request_id = COALESCE(?, deploy_request_id),
		  version = COALESCE(?, version),
		  error = ?,
		  message = COALESCE(?, message),
		  finished_at = CASE WHEN ? IN ('succeeded','failed') THEN ? ELSE finished_at END
		WHERE request_id = ?`,
		string(patch.State),
		nullIfEmpty(patch.Deployment),
		nullIfEmpty(patch.DeployRequestID),
		nullIfEmpty(patch.Version),
		nullIfEmpty(patch.Error),
		nullIfEmpty(patch.Message),
		string(patch.State),
		nowISO(),
		requestID,
	)
	return err
}

func (s *Store) ListPipelinesByState(state PipelineState) ([]PipelineJob, error) {
	rows, err := s.db.Query(`
		SELECT request_id, service_id, ref, state, deployment, deploy_request_id,
		       version, error, message, requested_at, started_at, finished_at
		FROM pipelines WHERE state = ? ORDER BY requested_at ASC`, string(state))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipelineJob
	for rows.Next() {
		job, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []PipelineJob{}
	}
	return out, rows.Err()
}

func scanPipeline(row scannable) (*PipelineJob, error) {
	var job PipelineJob
	var deployment, deployID, version, errStr, message, started, finished sql.NullString
	var state string
	err := row.Scan(
		&job.RequestID, &job.ServiceID, &job.Ref, &state,
		&deployment, &deployID, &version, &errStr, &message,
		&job.RequestedAt, &started, &finished,
	)
	if err != nil {
		return nil, err
	}
	job.State = PipelineState(state)
	job.Deployment = deployment.String
	job.DeployRequestID = deployID.String
	job.Version = version.String
	job.Error = errStr.String
	job.Message = message.String
	job.StartedAt = started.String
	job.FinishedAt = finished.String
	return &job, nil
}

func failPipeline(store *Store, requestID, msg string) {
	_ = store.UpdatePipeline(requestID, PipelineJob{
		State:   PipelineFailed,
		Error:   msg,
		Message: "pipeline failed",
	})
	_ = store.AddPipelineEvent(requestID, "error", msg)
	fmt.Printf("[pipeline] %s failed: %s\n", requestID, msg)
}
