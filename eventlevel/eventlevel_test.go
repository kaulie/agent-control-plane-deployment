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
		{name: "unknown is preserved", level: "custom", want: Level("custom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.level); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.level, got, tc.want)
			}
		})
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
