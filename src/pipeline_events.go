package main

import "github.com/kaulie/agent-control-plane-deployment/eventlevel"

// PipelineEvent is a single timestamped log line attached to a pipeline job.
// Events are appended as the pipeline progresses (queued → packaging →
// deploying → succeeded/failed) so the detail view can show a timeline.
type PipelineEvent struct {
	ID        int64  `json:"id"`
	RequestID string `json:"requestId"`
	Ts        string `json:"ts"`
	Level     string `json:"level"` // eventlevel.Info | Success | Warn | Error
	Message   string `json:"message"`
}

func (s *Store) migratePipelineEvents() error {
	if _, err := s.db.Exec(`
      CREATE TABLE IF NOT EXISTS pipeline_events (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        request_id TEXT NOT NULL,
        ts TEXT NOT NULL,
        level TEXT NOT NULL DEFAULT '` + string(eventlevel.Info) + `',
        message TEXT NOT NULL
      );
      CREATE INDEX IF NOT EXISTS idx_pipeline_events_request
        ON pipeline_events(request_id, id);
    `); err != nil {
		return err
	}
	// Migrate any legacy/non-canonical level names to the canonical set:
	// "ok" stays meaningful as a success, every other unknown name becomes info.
	if err := s.migrateEventLevels("pipeline_events"); err != nil {
		return err
	}
	return nil
}

// AddPipelineEvent appends one event row. Level names are normalized to the
// canonical eventlevel set before they are written.
func (s *Store) AddPipelineEvent(requestID string, level eventlevel.Level, message string) error {
	level = eventlevel.Normalize(string(level))
	_, err := s.db.Exec(
		`INSERT INTO pipeline_events (request_id, ts, level, message) VALUES (?, ?, ?, ?)`,
		requestID, nowISO(), string(level), message,
	)
	return err
}

// ListPipelineEvents returns events for a pipeline ordered by insertion (id ASC).
func (s *Store) ListPipelineEvents(requestID string) ([]PipelineEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, request_id, ts, level, message
		 FROM pipeline_events WHERE request_id = ? ORDER BY id ASC`,
		requestID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipelineEvent
	for rows.Next() {
		var ev PipelineEvent
		if err := rows.Scan(&ev.ID, &ev.RequestID, &ev.Ts, &ev.Level, &ev.Message); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if out == nil {
		out = []PipelineEvent{}
	}
	return out, rows.Err()
}
