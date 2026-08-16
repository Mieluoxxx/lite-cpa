package reqlog

import (
	"fmt"
	"strings"
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
	if len(parts) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}
