package eventlevel

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  Level
	}{
		{name: "empty defaults to info", level: "", want: Info},
		{name: "info", level: "info", want: Info},
		{name: "legacy ok maps to success", level: "ok", want: Success},
		{name: "success", level: "success", want: Success},
		{name: "warn", level: "warn", want: Warn},
		{name: "error", level: "error", want: Error},
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
