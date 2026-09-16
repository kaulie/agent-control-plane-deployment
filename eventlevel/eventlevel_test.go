package eventlevel

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  Level
	}{
		{name: "empty defaults to info", level: "", want: Info},
		{name: "info", level: string(Info), want: Info},
		{name: "legacy ok maps to success", level: LegacySuccessAlias, want: Success},
		{name: "success", level: string(Success), want: Success},
		{name: "warn", level: string(Warn), want: Warn},
		{name: "error", level: string(Error), want: Error},
		{name: "uppercase info maps to info", level: "INFO", want: Info},
		{name: "uppercase legacy ok maps to success", level: "OK", want: Success},
		{name: "padded warn maps to warn", level: "  warn  ", want: Warn},
		{name: "unknown falls back to info", level: "custom", want: Info},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.level); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.level, got, tc.want)
			}
		})
	}
}

func TestCanonicalNames(t *testing.T) {
	got := CanonicalNames()
	want := []string{"info", "success", "warn", "error"}
	if len(got) != len(want) {
		t.Fatalf("CanonicalNames() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CanonicalNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCanonicalLevelNames(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "info", got: string(Info), want: "info"},
		{name: "success", got: string(Success), want: "success"},
		{name: "warn", got: string(Warn), want: "warn"},
		{name: "error", got: string(Error), want: "error"},
		{name: "legacy success alias", got: LegacySuccessAlias, want: "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %q, want canonical name %q", tc.name, tc.got, tc.want)
			}
		})
	}
}
