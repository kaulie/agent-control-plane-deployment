package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/kaulie/agent-control-plane-deployment/pgdb"
	_ "modernc.org/sqlite"
)

// legacySQLiteTables is the copy order for deploy.sqlite. There are no foreign
// keys; events come after the jobs they belong to so a partial read is still
// understandable in logs. sqlite_sequence is SQLite bookkeeping and is not copied.
var legacySQLiteTables = []string{
	"services",
	"deploys",
	"pipelines",
	"artifacts",
	"deploy_events",
	"pipeline_events",
	"meta",
}

// identityTables need their sequences moved past the imported ids.
var identityTables = []string{"artifacts", "deploy_events", "pipeline_events"}

// ImportLegacySQLite copies deploy.sqlite into this database once.
//
// The first start against an empty PostgreSQL database imports the file when
// it exists, then records meta.sqlite_imported_v1. Later starts do not read
// the file again, so deleting a service in PostgreSQL does not resurrect it
// from SQLite. A missing file is not an error and is not recorded, so an
// import can still happen if the file shows up before any rows are written.
// A non-empty database is left alone and the import is marked done.
func (s *Store) ImportLegacySQLite(path string) (bool, error) {
	var done string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'sqlite_imported_v1'`).Scan(&done)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	nonempty, err := s.legacyImportTargetNonEmpty()
	if err != nil {
		return false, err
	}
	if nonempty {
		if err := s.markSQLiteImported("skipped-nonempty"); err != nil {
			return false, err
		}
		return false, nil
	}

	src, err := openLegacySQLite(path)
	if err != nil {
		return false, err
	}
	defer src.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range legacySQLiteTables {
		if err := copyLegacyTable(tx, src, table); err != nil {
			return false, err
		}
	}
	for _, table := range identityTables {
		if err := resetIdentitySequence(tx, table); err != nil {
			return false, err
		}
	}
	// migrate() already rewrote levels, but the table was empty then. Rows
	// copied from SQLite still carry the legacy "ok" alias.
	for _, table := range []string{"deploy_events", "pipeline_events"} {
		if err := rewriteEventLevels(tx, table); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO meta (k, v) VALUES ('sqlite_imported_v1', ?)`,
		nowISO()+" imported",
	); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) legacyImportTargetNonEmpty() (bool, error) {
	for _, table := range []string{"services", "deploys", "pipelines", "artifacts"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) markSQLiteImported(note string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (k, v) VALUES ('sqlite_imported_v1', ?)
		 ON CONFLICT (k) DO NOTHING`,
		nowISO()+" "+note,
	)
	return err
}

func openLegacySQLite(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open legacy sqlite: %w", err)
	}
	return db, nil
}

func copyLegacyTable(tx *pgdb.Tx, src *sql.DB, table string) error {
	var one int
	err := src.QueryRow(`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&one)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	srcCols, err := sqliteColumnNames(src, table)
	if err != nil {
		return err
	}
	dstCols, err := postgresColumnNames(tx, table)
	if err != nil {
		return err
	}
	dstSet := map[string]bool{}
	for _, c := range dstCols {
		dstSet[c] = true
	}
	var cols []string
	for _, c := range srcCols {
		if dstSet[c] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return nil
	}

	rows, err := src.Query(`SELECT ` + strings.Join(cols, ", ") + ` FROM ` + table)
	if err != nil {
		return fmt.Errorf("read sqlite %s: %w", table, err)
	}
	defer rows.Close()

	var batch [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i, v := range vals {
			vals[i] = normalizeSQLiteValue(v)
		}
		batch = append(batch, vals)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return insertLegacyRows(tx, table, cols, batch)
}

func sqliteColumnNames(src *sql.DB, table string) ([]string, error) {
	rows, err := src.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func postgresColumnNames(tx *pgdb.Tx, table string) ([]string, error) {
	rows, err := tx.Query(
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = ?
		 ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func insertLegacyRows(tx *pgdb.Tx, table string, cols []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	const chunk = 100
	colList := strings.Join(cols, ", ")
	conflict := ""
	if table == "meta" {
		conflict = " ON CONFLICT (k) DO NOTHING"
	}
	for start := 0; start < len(rows); start += chunk {
		end := start + chunk
		if end > len(rows) {
			end = len(rows)
		}
		part := rows[start:end]
		var b strings.Builder
		b.WriteString("INSERT INTO ")
		b.WriteString(table)
		b.WriteString(" (")
		b.WriteString(colList)
		b.WriteString(") VALUES ")
		args := make([]any, 0, len(part)*len(cols))
		for i, row := range part {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('(')
			for j := range cols {
				if j > 0 {
					b.WriteByte(',')
				}
				b.WriteByte('?')
				args = append(args, row[j])
			}
			b.WriteByte(')')
		}
		b.WriteString(conflict)
		if _, err := tx.Exec(b.String(), args...); err != nil {
			return fmt.Errorf("insert %s: %w", table, err)
		}
	}
	return nil
}

func resetIdentitySequence(tx *pgdb.Tx, table string) error {
	_, err := tx.Exec(
		`SELECT setval(pg_get_serial_sequence('` + table + `', 'id'), s.m, true)
		 FROM (SELECT MAX(id) AS m FROM ` + table + `) s
		 WHERE s.m IS NOT NULL`,
	)
	if err != nil {
		return fmt.Errorf("reset %s id sequence: %w", table, err)
	}
	return nil
}

func normalizeSQLiteValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	default:
		return v
	}
}
