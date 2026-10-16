package pgdb

import "testing"

func TestRebindLeavesQuotedQuestionMarks(t *testing.T) {
	got := rebind("SELECT '?', 'it''s ?', ? , ?")
	want := "SELECT '?', 'it''s ?', $1 , $2"
	if got != want {
		t.Fatalf("rebind = %q, want %q", got, want)
	}
}

func TestSplitStatementsKeepsSemicolonInsideString(t *testing.T) {
	parts := splitStatements("CREATE TABLE t (a TEXT DEFAULT ''); INSERT INTO t VALUES ('a;b');")
	if len(parts) != 2 {
		t.Fatalf("parts = %#v, want 2", parts)
	}
	if parts[1] != "INSERT INTO t VALUES ('a;b')" {
		t.Fatalf("second = %q", parts[1])
	}
}
