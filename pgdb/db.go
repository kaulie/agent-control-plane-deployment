// Package pgdb opens the deployment control plane's PostgreSQL database.
//
// Call sites keep writing SQLite-style "?" placeholders. Exec/Query/QueryRow
// rewrite them to PostgreSQL "$1" placeholders so the rest of the server does
// not have to.
package pgdb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// EnvDSN is the connection string for the deployment database.
const EnvDSN = "DEPLOYMENT_POSTGRES_DSN"

// DB is a *sql.DB that rewrites "?" placeholders and splits multi-statement
// Exec calls (schema migration still sends several DDL statements at once).
type DB struct {
	*sql.DB
}

// Tx is a *sql.Tx with the same placeholder rewriting.
type Tx struct {
	*sql.Tx
}

// Open connects to dsn and pings it. A non-standard "search_path" query
// parameter is applied as a startup parameter and removed before libpq parses
// the URL, so tests can isolate themselves in a private schema.
func Open(dsn string) (*DB, error) {
	cfg, err := parseConfig(dsn)
	if err != nil {
		return nil, err
	}
	sqlDB := stdlib.OpenDB(*cfg)
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return &DB{DB: sqlDB}, nil
}

// LoadDSN returns DEPLOYMENT_POSTGRES_DSN, or the same variable from
// $DEPLOYMENT_HOME/data/postgres.env, or from ~/database/deployment/pg.env.
// A missing file is not an error; an unreadable file is.
func LoadDSN() (string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvDSN)); v != "" {
		return v, nil
	}
	for _, path := range dsnFiles() {
		dsn, ok, err := readDSNFile(path)
		if err != nil {
			return "", err
		}
		if ok {
			return dsn, nil
		}
	}
	return "", nil
}

// RedactedTarget returns host/db for logs. It never includes the userinfo.
func RedactedTarget(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "postgres"
	}
	return u.Host + u.Path
}

func dsnFiles() []string {
	var out []string
	if home := strings.TrimSpace(os.Getenv("DEPLOYMENT_HOME")); home != "" {
		out = append(out, filepath.Join(home, "data", "postgres.env"))
	}
	if userHome, err := os.UserHomeDir(); err == nil && userHome != "" {
		out = append(out, filepath.Join(userHome, "database", "deployment", "pg.env"))
	}
	return out
}

func readDSNFile(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != EnvDSN {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '\'' && val[len(val)-1] == '\'') || (val[0] == '"' && val[len(val)-1] == '"') {
				val = val[1 : len(val)-1]
			}
		}
		if val = strings.TrimSpace(val); val != "" {
			return val, true, nil
		}
	}
	return "", false, nil
}

func parseConfig(dsn string) (*pgx.ConnConfig, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("postgres DSN is empty")
	}
	var schema string
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		schema = q.Get("search_path")
		if schema != "" {
			if !validSchema(schema) {
				return nil, fmt.Errorf("invalid search_path %q", schema)
			}
			q.Del("search_path")
			u.RawQuery = q.Encode()
			dsn = u.String()
		}
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if schema != "" {
		cfg.RuntimeParams["search_path"] = schema
	}
	return cfg, nil
}

func validSchema(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// Exec rewrites placeholders. Several statements separated by semicolons run
// one at a time; that form does not accept arguments.
func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	if d == nil || d.DB == nil {
		return nil, fmt.Errorf("nil db")
	}
	return exec(d.DB.Exec, query, args...)
}

// Query rewrites placeholders.
func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	if d == nil || d.DB == nil {
		return nil, fmt.Errorf("nil db")
	}
	return d.DB.Query(rebind(query), args...)
}

// QueryRow rewrites placeholders.
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	if d == nil || d.DB == nil {
		return nil
	}
	return d.DB.QueryRow(rebind(query), args...)
}

// Begin starts a transaction whose Exec/Query/QueryRow also rewrite placeholders.
func (d *DB) Begin() (*Tx, error) {
	if d == nil || d.DB == nil {
		return nil, fmt.Errorf("nil db")
	}
	tx, err := d.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx}, nil
}

// Exec rewrites placeholders.
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	if t == nil || t.Tx == nil {
		return nil, fmt.Errorf("nil tx")
	}
	return exec(t.Tx.Exec, query, args...)
}

// Query rewrites placeholders.
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	if t == nil || t.Tx == nil {
		return nil, fmt.Errorf("nil tx")
	}
	return t.Tx.Query(rebind(query), args...)
}

// QueryRow rewrites placeholders.
func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	if t == nil || t.Tx == nil {
		return nil
	}
	return t.Tx.QueryRow(rebind(query), args...)
}

type execFunc func(query string, args ...any) (sql.Result, error)

func exec(run execFunc, query string, args ...any) (sql.Result, error) {
	parts := splitStatements(query)
	if len(parts) == 0 {
		return nil, nil
	}
	if len(parts) == 1 {
		return run(rebind(parts[0]), args...)
	}
	if len(args) != 0 {
		return nil, fmt.Errorf("pgdb: multi-statement Exec does not take arguments")
	}
	var last sql.Result
	for _, part := range parts {
		res, err := run(part)
		if err != nil {
			return nil, fmt.Errorf("%w\nstatement: %s", err, truncate(part, 240))
		}
		last = res
	}
	return last, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// rebind turns "?" placeholders into "$1" "$2" ... "?" inside single-quoted
// strings (including ” escapes) is left alone.
func rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inSingle := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '\'' {
			b.WriteByte(c)
			if inSingle && i+1 < len(query) && query[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			inSingle = !inSingle
			continue
		}
		if c == '?' && !inSingle {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func splitStatements(query string) []string {
	var parts []string
	var b strings.Builder
	inSingle := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '\'' {
			b.WriteByte(c)
			if inSingle && i+1 < len(query) && query[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			inSingle = !inSingle
			continue
		}
		if c == ';' && !inSingle {
			if s := strings.TrimSpace(b.String()); s != "" {
				parts = append(parts, s)
			}
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		parts = append(parts, s)
	}
	return parts
}
