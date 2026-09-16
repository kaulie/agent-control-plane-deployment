package main

import "strings"

// ListFilter is the shared filter + pagination for the two history lists
// (GET /api/deploys and GET /api/pipelines). It is deliberately flat so the
// panel can send one set of query params to either endpoint; fields that do
// not apply to a given table are simply ignored by that table's builder.
type ListFilter struct {
	ServiceID       string `json:"serviceId,omitempty"`
	State           string `json:"state,omitempty"`
	TriggeredByRole string `json:"triggeredByRole,omitempty"`
	TriggeredByID   string `json:"triggeredById,omitempty"`
	Deployment      string `json:"deployment,omitempty"`
	Version         string `json:"version,omitempty"`
	Ref             string `json:"ref,omitempty"`
	Keyword         string `json:"q,omitempty"`
	// From / To bound requested_at (inclusive), as ISO date or date-time
	// strings; both are optional. The panel sends full-day bounds.
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Page     int    `json:"page"`
	PageSize int    `json:"pageSize"`
}

const (
	defaultPageSize = 20
	maxPageSize     = 200
)

// normalize clamps page/pageSize into sane bounds (idempotent).
func (f *ListFilter) normalize() {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = defaultPageSize
	}
	if f.PageSize > maxPageSize {
		f.PageSize = maxPageSize
	}
}

func (f ListFilter) offset() int { return (f.Page - 1) * f.PageSize }

// wb accumulates "WHERE" conditions and their bound args for a history query.
type wb struct {
	conds []string
	args  []any
}

func (w *wb) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

// eq adds "<col> = ?" when val is non-empty (exact match).
func (w *wb) eq(col, val string) {
	if val != "" {
		w.add(col+" = ?", val)
	}
}

// like adds "<col> LIKE %val%" when val is non-empty (contains match).
func (w *wb) like(col, val string) {
	if val != "" {
		w.add(col+" LIKE ?", "%"+val+"%")
	}
}

// keyword matches a free-text term against several columns (OR).
func (w *wb) keyword(cols []string, kw string) {
	if kw == "" || len(cols) == 0 {
		return
	}
	parts := make([]string, 0, len(cols))
	args := make([]any, 0, len(cols))
	like := "%" + kw + "%"
	for _, c := range cols {
		parts = append(parts, "COALESCE("+c+",'') LIKE ?")
		args = append(args, like)
	}
	w.add("("+strings.Join(parts, " OR ")+")", args...)
}

// timeRange bounds requested_at inclusively (either end optional).
func (w *wb) timeRange(from, to string) {
	if from != "" {
		w.add("requested_at >= ?", from)
	}
	if to != "" {
		w.add("requested_at <= ?", to)
	}
}

// where renders the accumulated conditions (" WHERE a AND b") or "".
func (w *wb) where() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// baseFilter fills the conditions shared by deploys and pipelines.
func (f ListFilter) baseFilter() *wb {
	w := &wb{}
	w.eq("service_id", f.ServiceID)
	w.eq("state", f.State)
	w.eq("triggered_by_role", f.TriggeredByRole)
	w.eq("triggered_by_id", f.TriggeredByID)
	w.like("deployment", f.Deployment)
	w.like("version", f.Version)
	w.timeRange(f.From, f.To)
	return w
}

// countRows returns COUNT(*) for table with the accumulated WHERE clause.
func (s *Store) countRows(table string, w *wb) (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM "+table+w.where(), w.args...).Scan(&n)
	return n, err
}

// ListDeploysFiltered returns one page of deploys matching f plus the total
// number of matching rows (for the panel's pager).
func (s *Store) ListDeploysFiltered(f ListFilter) ([]DeployJob, int, error) {
	f.normalize()
	w := f.baseFilter()
	w.keyword([]string{"request_id", "service_id", "deployment", "version", "message", "error", "triggered_by_id"}, f.Keyword)

	total, err := s.countRows("deploys", w)
	if err != nil {
		return nil, 0, err
	}
	args := append(append([]any{}, w.args...), f.PageSize, f.offset())
	rows, err := s.db.Query(
		"SELECT "+deployColumns+" FROM deploys"+w.where()+
			" ORDER BY requested_at DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []DeployJob{}
	for rows.Next() {
		job, err := scanDeployRows(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *job)
	}
	return out, total, rows.Err()
}

// ListPipelinesFiltered returns one page of pipelines matching f plus the
// total number of matching rows. ref is pipeline-only.
func (s *Store) ListPipelinesFiltered(f ListFilter) ([]PipelineJob, int, error) {
	f.normalize()
	w := f.baseFilter()
	w.like("ref", f.Ref)
	w.keyword([]string{"request_id", "service_id", "ref", "deployment", "version", "message", "error", "triggered_by_id"}, f.Keyword)

	total, err := s.countRows("pipelines", w)
	if err != nil {
		return nil, 0, err
	}
	args := append(append([]any{}, w.args...), f.PageSize, f.offset())
	rows, err := s.db.Query(
		"SELECT "+pipelineColumns+" FROM pipelines"+w.where()+
			" ORDER BY requested_at DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []PipelineJob{}
	for rows.Next() {
		job, err := scanPipeline(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *job)
	}
	return out, total, rows.Err()
}
