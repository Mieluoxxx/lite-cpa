package reqlog

import (
	"strings"
)

// buildListWhere builds a SQLite WHERE clause and its args.
func buildListWhere(f ListFilter) (string, []any) {
	var parts []string
	var args []any
	add := func(clause string, v any) {
		parts = append(parts, clause)
		args = append(args, v)
	}

	if f.Model != "" {
		add("model = ?", f.Model)
	}
	if f.Upstream != "" {
		add("upstream = ?", f.Upstream)
	}
	if f.Protocol != "" {
		add("protocol = ?", f.Protocol)
	}
	if f.Status != "" {
		// Accept "4xx", "5xx", "2" etc. — trim suffix and use LIKE.
		prefix := strings.TrimSuffix(f.Status, "xx")
		add("CAST(status_code AS TEXT) LIKE ?", prefix+"%")
	}
	if f.Outcome != "" {
		add("outcome = ?", f.Outcome)
	}
	if f.ErrorsOnly {
		parts = append(parts, `outcome = 'error'`)
	}
	if !f.From.IsZero() {
		add("ts >= ?", f.From.UTC().UnixNano())
	}
	if !f.To.IsZero() {
		add("ts < ?", f.To.UTC().UnixNano())
	}
	if f.UserAgents != nil {
		if len(f.UserAgents) == 0 {
			parts = append(parts, "1 = 0")
		} else {
			marks := make([]string, len(f.UserAgents))
			for i, ua := range f.UserAgents {
				args = append(args, ua)
				marks[i] = "?"
			}
			parts = append(parts, "user_agent IN ("+strings.Join(marks, ", ")+")")
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}
