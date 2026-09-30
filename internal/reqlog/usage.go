package reqlog

import (
	"context"
	"sort"
	"time"
)

// UsageBucket is one usage_hourly row: totals for a UTC hour, client
// User-Agent and model. Requests/Success/Errors/Canceled/Incomplete count only
// final rows (Retried=false); Attempts counts every row.
type UsageBucket struct {
	Hour         int64  `json:"hour,omitempty"` // unix seconds, start of the UTC hour
	UserAgent    string `json:"user_agent"`
	Model        string `json:"model"`
	Attempts     int64  `json:"attempts"`
	Requests     int64  `json:"requests"`
	Success      int64  `json:"success"`
	Errors       int64  `json:"errors"`
	Canceled     int64  `json:"canceled"`
	Incomplete   int64  `json:"incomplete"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
	LastSeenMS   int64  `json:"last_seen_ms"`
}

// usageDelta converts one record into the increment it adds to usage_hourly.
func usageDelta(r Record) UsageBucket {
	b := UsageBucket{
		Hour:       r.Timestamp.UTC().Unix() / 3600 * 3600,
		UserAgent:  r.UserAgent,
		Model:      r.Model,
		Attempts:   1,
		LastSeenMS: r.Timestamp.UnixMilli(),
	}
	if !r.Retried {
		b.Requests = 1
		switch r.Outcome {
		case OutcomeCompleted:
			b.Success = 1
		case OutcomeError:
			b.Errors = 1
		case OutcomeClientCanceled:
			b.Canceled = 1
		}
		if !r.UsageComplete {
			b.Incomplete = 1
		}
	}
	if r.UsageComplete {
		b.InputTokens = r.InputTokens
		b.OutputTokens = r.OutputTokens
		b.CachedTokens = r.CachedTokens
	}
	return b
}

func (b *UsageBucket) add(o UsageBucket) {
	b.Attempts += o.Attempts
	b.Requests += o.Requests
	b.Success += o.Success
	b.Errors += o.Errors
	b.Canceled += o.Canceled
	b.Incomplete += o.Incomplete
	b.InputTokens += o.InputTokens
	b.OutputTokens += o.OutputTokens
	b.CachedTokens += o.CachedTokens
	if o.LastSeenMS > b.LastSeenMS {
		b.LastSeenMS = o.LastSeenMS
	}
}

// DailyUsage is usage for one local day, User-Agent and model.
type DailyUsage struct {
	Day string `json:"day"` // YYYY-MM-DD in the caller's UTC offset
	UsageBucket
}

// WeekHourUsage is usage for one local weekday/hour slot and User-Agent,
// summed across models and weeks.
type WeekHourUsage struct {
	Weekday   int `json:"weekday"` // 0 = Monday
	HourOfDay int `json:"hour_of_day"`
	UsageBucket
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func dayLabel(day int64) string {
	return time.Unix(day*86400, 0).UTC().Format("2006-01-02")
}

// UsageWindow returns the UTC instant at which the local day days-1 before
// today starts, plus the first and last local day labels of the window.
func UsageWindow(now time.Time, days int, offsetSeconds int64) (from time.Time, firstDay, lastDay string) {
	today := floorDiv(now.Unix()+offsetSeconds, 86400)
	first := today - int64(days) + 1
	return time.Unix(first*86400-offsetSeconds, 0), dayLabel(first), dayLabel(today)
}

// AggregateDaily folds hourly buckets into local days for a fixed UTC offset.
// Offsets that are not whole hours shift by up to 30 minutes, which is the
// price of hourly storage.
func AggregateDaily(buckets []UsageBucket, offsetSeconds int64) []DailyUsage {
	type key struct {
		day       int64
		userAgent string
		model     string
	}
	groups := make(map[key]*DailyUsage)
	for _, b := range buckets {
		k := key{floorDiv(b.Hour+offsetSeconds, 86400), b.UserAgent, b.Model}
		g, ok := groups[k]
		if !ok {
			g = &DailyUsage{Day: dayLabel(k.day)}
			g.UserAgent, g.Model = b.UserAgent, b.Model
			groups[k] = g
		}
		g.add(b)
	}
	out := make([]DailyUsage, 0, len(groups))
	for _, g := range groups {
		g.Hour = 0
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		if out[i].UserAgent != out[j].UserAgent {
			return out[i].UserAgent < out[j].UserAgent
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// AggregateWeekHour folds hourly buckets into local weekday x hour slots.
func AggregateWeekHour(buckets []UsageBucket, offsetSeconds int64) []WeekHourUsage {
	type key struct {
		weekday   int
		hour      int
		userAgent string
	}
	groups := make(map[key]*WeekHourUsage)
	for _, b := range buckets {
		local := b.Hour + offsetSeconds
		day := floorDiv(local, 86400)
		// 1970-01-01 was a Thursday, index 3 when Monday is 0.
		k := key{int((day%7 + 7 + 3) % 7), int((local - day*86400) / 3600), b.UserAgent}
		g, ok := groups[k]
		if !ok {
			g = &WeekHourUsage{Weekday: k.weekday, HourOfDay: k.hour}
			g.UserAgent = b.UserAgent
			groups[k] = g
		}
		g.add(b)
	}
	out := make([]WeekHourUsage, 0, len(groups))
	for _, g := range groups {
		g.Hour = 0
		g.Model = ""
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weekday != out[j].Weekday {
			return out[i].Weekday < out[j].Weekday
		}
		if out[i].HourOfDay != out[j].HourOfDay {
			return out[i].HourOfDay < out[j].HourOfDay
		}
		return out[i].UserAgent < out[j].UserAgent
	})
	return out
}

// Usage returns hourly rollup buckets when logging is enabled.
func (l *Logger) Usage(ctx context.Context, from, to time.Time) ([]UsageBucket, error) {
	if l == nil || !l.Enabled() {
		return []UsageBucket{}, nil
	}
	return l.store.Usage(ctx, from, to)
}
