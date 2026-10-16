package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/kaulie/agent-control-plane-deployment/pgdb"
)

func TestMain(m *testing.M) {
	dropLeftoverTestSchemas()
	os.Exit(m.Run())
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	_, dsn := openTestSchema(t)
	s, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openTestSchema creates a private schema on the deployment database and
// returns a connection plus a DSN whose search_path is that schema.
func openTestSchema(t *testing.T) (*pgdb.DB, string) {
	t.Helper()
	base := deploymentTestDSN(t)
	schema := "t_" + randomSchemaID(t)
	admin, err := pgdb.Open(base)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	})

	dsn := dsnWithSearchPath(t, base, schema)
	raw, err := pgdb.Open(dsn)
	if err != nil {
		t.Fatalf("open schema: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	var got string
	if err := raw.QueryRow("SELECT current_schema()").Scan(&got); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if got != schema {
		t.Fatalf("current_schema = %q, want %q (refusing to touch another schema)", got, schema)
	}
	return raw, dsn
}

func deploymentTestDSN(t *testing.T) string {
	t.Helper()
	dsn, err := pgdb.LoadDSN()
	if err != nil {
		t.Fatalf("load postgres DSN: %v", err)
	}
	if dsn == "" {
		t.Fatal("DEPLOYMENT_POSTGRES_DSN is empty; write ~/database/deployment/pg.env")
	}
	return dsn
}

func dsnWithSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func randomSchemaID(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func dropLeftoverTestSchemas() {
	dsn, err := pgdb.LoadDSN()
	if err != nil || dsn == "" {
		return
	}
	db, err := pgdb.Open(dsn)
	if err != nil {
		return
	}
	defer db.Close()
	rows, err := db.Query(`SELECT nspname FROM pg_namespace WHERE nspname LIKE 't\_%' ESCAPE '\'`)
	if err != nil {
		return
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return
	}
	for _, name := range names {
		_, _ = db.Exec("DROP SCHEMA IF EXISTS " + name + " CASCADE")
	}
}
