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

	// LegacySuccessAlias is the deprecated pre-standardization name that used
	// to be stored for successful events. It is kept so Normalize and the
	// event-table migrations reference that legacy name from one definition
	// instead of hard-coding "ok".
	LegacySuccessAlias = "ok"
)

// CanonicalNames returns the only level names the deployment pipeline should
// emit, store, or consume, in their canonical lowercase form.
func CanonicalNames() []string {
	return []string{string(Info), string(Success), string(Warn), string(Error)}
}

// Normalize maps every level name to the canonical set: an empty level and any
// unknown value fall back to Info, and the legacy "ok" alias maps to Success.
// This keeps the same convention used by the server, the upgrader, and the web
// panel, so no inconsistent event-level names are written to either event
// table.
func Normalize(level string) Level {
	switch level {
	case string(Info):
		return Info
	case LegacySuccessAlias, string(Success):
		return Success
	case string(Warn):
		return Warn
	case string(Error):
		return Error
	default:
		return Info
	}
}
