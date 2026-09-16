package main

import (
	"strings"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

// DeployEvent is a single timestamped log line attached to a deploy job,
// recorded by executeDeploy (rsync / restart / health) and, for self-deploys,
// by the acp-upgrader process (stop / start / health).
type DeployEvent struct {
	ID        int64  `json:"id"`
	RequestID string `json:"requestId"`
	Ts        string `json:"ts"`
	Level     string `json:"level"` // eventlevel.Info | Success | Warn | Error
	Message   string `json:"message"`
}

func (s *Store) migrateDeployEvents() error {
	if _, err := s.db.Exec(`
      CREATE TABLE IF NOT EXISTS deploy_events (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        request_id TEXT NOT NULL,
        ts TEXT NOT NULL,
        level TEXT NOT NULL DEFAULT '` + string(eventlevel.Info) + `',
        message TEXT NOT NULL
      );
      CREATE INDEX IF NOT EXISTS idx_deploy_events_request
        ON deploy_events(request_id, id);
    `); err != nil {
		return err
	}
	// Migrate any legacy/non-canonical level names to the canonical set:
	// "ok" stays meaningful as a success, every other unknown name becomes info.
	if err := s.migrateEventLevels("deploy_events"); err != nil {
		return err
	}
	return nil
}

// migrateEventLevels rewrites a shared event table so only the canonical
// eventlevel names remain. The legacy success alias is preserved as success;
// every other non-canonical value is collapsed to info. Table names are fixed
// internal identifiers, never user input.
func (s *Store) migrateEventLevels(table string) error {
	if _, err := s.db.Exec(
		`UPDATE `+table+` SET level = ? WHERE level = ?`,
		string(eventlevel.Success), eventlevel.LegacySuccessAlias,
	); err != nil {
		return err
	}

	canonical := eventlevel.CanonicalNames()
	placeholders := make([]string, len(canonical))
	args := make([]any, 0, len(canonical)+1)
	args = append(args, string(eventlevel.Info))
	for i, name := range canonical {
		placeholders[i] = "?"
		args = append(args, name)
	}
	_, err := s.db.Exec(
		`UPDATE `+table+` SET level = ? WHERE level NOT IN (`+
			strings.Join(placeholders, ", ")+`)`,
		args...,
	)
	return err
}

// AddDeployEvent appends one event row. Level names are normalized to the
// canonical eventlevel set before they are written.
func (s *Store) AddDeployEvent(requestID string, level eventlevel.Level, message string) error {
	level = eventlevel.Normalize(string(level))
	_, err := s.db.Exec(
		`INSERT INTO deploy_events (request_id, ts, level, message) VALUES (?, ?, ?, ?)`,
		requestID, nowISO(), string(level), message,
	)
	return err
}

// ListDeployEvents returns events for a deploy ordered by insertion (id ASC).
func (s *Store) ListDeployEvents(requestID string) ([]DeployEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, request_id, ts, level, message
		 FROM deploy_events WHERE request_id = ? ORDER BY id ASC`,
		requestID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployEvent
	for rows.Next() {
		var ev DeployEvent
		if err := rows.Scan(&ev.ID, &ev.RequestID, &ev.Ts, &ev.Level, &ev.Message); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if out == nil {
		out = []DeployEvent{}
	}
	return out, rows.Err()
}

// recordDeployEvent is a nil-safe helper for callers that may not have a Store
// (e.g. unit tests). It silently skips when store or requestID is empty.
func recordDeployEvent(store *Store, requestID string, level eventlevel.Level, message string) {
	if store == nil || requestID == "" {
		return
	}
	_ = store.AddDeployEvent(requestID, level, message)
}
