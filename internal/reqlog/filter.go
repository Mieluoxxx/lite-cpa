package reqlog

import (
	"fmt"
	"strings"
	"time"
)

// buildListWhere builds a WHERE clause and args.
// postgres=true uses $1-style placeholders; false uses ?.
func buildListWhere(f ListFilter, postgres bool) (string, []any) {
	var parts []string
	var args []any
	add := func(clause string, v any) {
		if postgres {
			parts = append(parts, fmt.Sprintf(clause, len(args)+1))
			args = append(args, v)
			return
		}
		// sqlite: clause uses ? already
		parts = append(parts, strings.ReplaceAll(clause, "$%d", "?"))
		args = append(args, v)
	}

	if f.Model != "" {
		add("model = $%d", f.Model)
	}
	if f.Upstream != "" {
		add("upstream = $%d", f.Upstream)
	}
	if f.Protocol != "" {
		add("protocol = $%d", f.Protocol)
	}
	if f.Status != "" {
		// Accept "4xx", "5xx", "2" etc. — trim suffix and use LIKE.
		prefix := strings.TrimSuffix(f.Status, "xx")
		add("CAST(status_code AS TEXT) LIKE $%d", prefix+"%")
	}
	if f.Outcome != "" {
		add("outcome = $%d", f.Outcome)
	}
	if f.ErrorsOnly {
		parts = append(parts, `outcome = 'error'`)
	}
	tsArg := func(t time.Time) any {
		if postgres {
			return t.UTC()
		}
		return t.UTC().UnixNano()
	}
	if !f.From.IsZero() {
		add("ts >= $%d", tsArg(f.From))
	}
	if !f.To.IsZero() {
		add("ts < $%d", tsArg(f.To))
	}
	if f.UserAgents != nil {
		if len(f.UserAgents) == 0 {
			parts = append(parts, "1 = 0")
		} else {
			marks := make([]string, len(f.UserAgents))
			for i, ua := range f.UserAgents {
				args = append(args, ua)
				if postgres {
					marks[i] = fmt.Sprintf("$%d", len(args))
				} else {
					marks[i] = "?"
				}
			}
			parts = append(parts, "user_agent IN ("+strings.Join(marks, ", ")+")")
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}
