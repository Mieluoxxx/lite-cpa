package translator

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// Severity splits conversion loss by whether it reaches the upstream model.
type Severity string

const (
	// LossGeneration marks a field that shapes what the model produces or what
	// it is allowed to do. The upstream request differs from what the client
	// asked for.
	LossGeneration Severity = "generation"
	// LossBookkeeping marks telemetry, storage hints and tracing ids the target
	// protocol has no home for. The model behaves the same either way.
	LossBookkeeping Severity = "bookkeeping"
)

// Diag is one source request field the conversion could not carry.
type Diag struct {
	Code     string   `json:"code"`
	Path     string   `json:"path,omitempty"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// fieldsWithSameRole lists source fields that different protocols spell
// differently. A field counts as carried when any name in its group survives,
// so a rename is never reported as a loss.
var fieldsWithSameRole = [][]string{
	{"messages", "input", "system", "instructions"},
	{"max_tokens", "max_completion_tokens", "max_output_tokens"},
	{"stop", "stop_sequences"},
	{"reasoning", "reasoning_effort", "thinking", "output_config"},
	{"tools", "functions"},
}

// lossCode names the diagnostic a dropped field produces. Fields missing from
// the table fall back to the generic code, so a translator that starts
// ignoring a new field still turns up in the log.
var lossCode = map[string]string{
	"response_format":      "response_format_unsupported",
	"text":                 "response_format_unsupported",
	"seed":                 "seed_unsupported",
	"n":                    "choice_count_unsupported",
	"temperature":          "temperature_unsupported",
	"top_p":                "top_p_unsupported",
	"top_k":                "top_k_unsupported",
	"logit_bias":           "logit_bias_unsupported",
	"logprobs":             "logprobs_unsupported",
	"presence_penalty":     "penalty_unsupported",
	"frequency_penalty":    "penalty_unsupported",
	"stop":                 "stop_unsupported",
	"stop_sequences":       "stop_unsupported",
	"tools":                "tools_unsupported",
	"functions":            "tools_unsupported",
	"tool_choice":          "tool_choice_unsupported",
	"parallel_tool_calls":  "parallel_tool_calls_unsupported",
	"reasoning":            "reasoning_unsupported",
	"reasoning_effort":     "reasoning_unsupported",
	"thinking":             "reasoning_unsupported",
	"output_config":        "reasoning_unsupported",
	"previous_response_id": "continuation_unsupported",
}

// bookkeeping lists fields whose loss never reaches the model.
var bookkeeping = map[string]bool{
	"user":              true,
	"metadata":          true,
	"store":             true,
	"include":           true,
	"truncation":        true,
	"service_tier":      true,
	"prompt_cache_key":  true,
	"safety_identifier": true,
}

// InspectRequestLoss reports the client request fields that the converted
// upstream body does not carry. It compares the payload with the body a
// translator produced, so it does not need to know how any translator works:
// a field the target protocol has no home for and a field a translator forgot
// to copy both show up. Use it only for cross-protocol conversion; same-format
// requests pass through untouched.
func InspectRequestLoss(from, to Format, source, target []byte) []Diag {
	if from == to || len(source) == 0 {
		return nil
	}
	root := gjson.ParseBytes(source)
	if !root.IsObject() {
		return nil
	}
	carried := carriedFields(target)
	var diags []Diag
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if meaningful(value) && !carried[name] {
			diags = append(diags, fieldLoss(name, to))
		}
		return true
	})
	return diags
}

// meaningful reports whether a field holds a value worth reporting. A field the
// client sent as null or empty is not a loss when the target omits it. It reads
// the raw literal instead of decoding the value, so a large body is never
// copied just to find out whether it is empty.
func meaningful(value gjson.Result) bool {
	if !value.Exists() || value.Type == gjson.Null {
		return false
	}
	raw := strings.TrimSpace(value.Raw)
	if value.Type == gjson.String || value.Type == gjson.JSON {
		// Raw keeps its delimiters, so "", [] and {} are two bytes long.
		return len(raw) > 2
	}
	return raw != ""
}

// carriedFields is the set of source field names the target body accounts for,
// including every alias of a field the target spelled differently.
func carriedFields(target []byte) map[string]bool {
	carried := make(map[string]bool)
	gjson.ParseBytes(target).ForEach(func(key, _ gjson.Result) bool {
		carried[key.String()] = true
		return true
	})
	for _, group := range fieldsWithSameRole {
		hit := false
		for _, name := range group {
			if carried[name] {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		for _, name := range group {
			carried[name] = true
		}
	}
	return carried
}

func fieldLoss(name string, to Format) Diag {
	code, ok := lossCode[name]
	if !ok {
		code = "field_unsupported"
	}
	severity := LossGeneration
	if bookkeeping[name] {
		severity = LossBookkeeping
	}
	return Diag{
		Code:     code,
		Path:     name,
		Severity: severity,
		Message:  fmt.Sprintf("%s is not carried by the %s request", name, to),
	}
}

// lossLimit caps how many distinct losses one request keeps. A malformed or
// unusually large request must not grow request state without bound.
const lossLimit = 32

// LossRecorder collects the losses of one client request. A retried request is
// converted once per attempt, so repeats are dropped and the total is capped.
type LossRecorder struct {
	seen      map[string]bool
	diags     []Diag
	truncated bool
}

func NewLossRecorder() *LossRecorder {
	return &LossRecorder{seen: make(map[string]bool)}
}

type lossRecorderKey struct{}

// WithLossRecorder arms ctx so RecordLosses files what a conversion drops.
func WithLossRecorder(ctx context.Context, rec *LossRecorder) context.Context {
	if rec == nil || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, lossRecorderKey{}, rec)
}

// RecordLosses compares a converted payload with the body it was converted from
// and files what the conversion dropped. Without a recorder in ctx it does
// nothing, so the executor never depends on one being armed.
func RecordLosses(ctx context.Context, from, to Format, source, target []byte) {
	if ctx == nil {
		return
	}
	rec, _ := ctx.Value(lossRecorderKey{}).(*LossRecorder)
	if rec == nil {
		return
	}
	for _, diag := range InspectRequestLoss(from, to, source, target) {
		rec.add(diag)
	}
}

func (r *LossRecorder) add(diag Diag) {
	if r == nil {
		return
	}
	key := diag.Code + "\x00" + diag.Path
	if r.seen[key] {
		return
	}
	if len(r.diags) >= lossLimit {
		r.truncated = true
		return
	}
	r.seen[key] = true
	r.diags = append(r.diags, diag)
}

func (r *LossRecorder) Diagnostics() []Diag {
	if r == nil || len(r.diags) == 0 {
		return nil
	}
	return append([]Diag(nil), r.diags...)
}

// Summary renders the distinct codes of the request as one log-safe line.
func (r *LossRecorder) Summary() string {
	if r == nil || len(r.diags) == 0 {
		return ""
	}
	generation, bookkeepingCount := 0, 0
	seen := make(map[string]bool, len(r.diags))
	for _, diag := range r.diags {
		if diag.Severity == LossGeneration {
			generation++
		} else {
			bookkeepingCount++
		}
		seen[diag.Code] = true
	}
	codes := make([]string, 0, len(seen))
	for code := range seen {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	summary := fmt.Sprintf("generation=%d bookkeeping=%d codes=%s", generation, bookkeepingCount, strings.Join(codes, ","))
	if r.truncated {
		summary += " truncated=true"
	}
	return summary
}
