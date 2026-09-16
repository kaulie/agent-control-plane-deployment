// Package eventlevel defines the canonical names for deployment pipeline
// event levels. Both the server (src) and the self-upgrade helper (upgrader)
// record into the same event tables, so the allowed level names live here to
// keep the naming convention consistent across binaries.
package eventlevel

// Level is a deployment pipeline event level.
type Level string

const (
	// Info marks an informational progression event.
	Info Level = "info"
	// Success marks a successfully completed step.
	Success Level = "success"
	// Warn marks a non-fatal problem (the pipeline/deploy continues).
	Warn Level = "warn"
	// Error marks a fatal problem for the current step.
	Error Level = "error"
)

// Normalize maps an empty level to Info and maps the legacy "ok" alias to
// Success. Unknown values are returned unchanged so callers never silently
// reclassify their data.
func Normalize(level string) Level {
	switch level {
	case "", string(Info):
		return Info
	case "ok", string(Success):
		return Success
	case string(Warn):
		return Warn
	case string(Error):
		return Error
	default:
		return Level(level)
	}
}
