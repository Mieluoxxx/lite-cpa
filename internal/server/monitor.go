package server

import (
	"embed"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/reqlog"
)

//go:embed dashboard
var dashboardFiles embed.FS

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	index, err := dashboardFiles.ReadFile("dashboard/index.html")
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(index)
}

func (s *Server) handleDashboardAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/dashboard/" {
		s.handleDashboard(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.FileServerFS(dashboardFiles).ServeHTTP(w, r)
}

func (s *Server) handleLogsStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	st, err := s.logger.Stats(r.Context(), logListFilter(r.URL.Query()))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleLogsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	q := r.URL.Query()
	f := logListFilter(q)
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Offset = n
		}
	}
	res, err := s.logger.List(r.Context(), f)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAffinityStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.affinity.Stats())
}

func (s *Server) handleRoutingStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.health.Snapshot()})
}

func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	deleted, err := s.logger.Clear(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": deleted})
}

func logListFilter(q url.Values) reqlog.ListFilter {
	f := reqlog.ListFilter{
		Model:      strings.TrimSpace(q.Get("model")),
		Upstream:   strings.TrimSpace(q.Get("upstream")),
		Protocol:   strings.TrimSpace(q.Get("protocol")),
		Status:     strings.TrimSpace(q.Get("status")),
		Outcome:    strings.TrimSpace(q.Get("outcome")),
		ErrorsOnly: truthy(q.Get("errors")),
		From:       unixParam(q.Get("from")),
		To:         unixParam(q.Get("to")),
	}
	// User-Agent values are matched verbatim, so they are not trimmed.
	if q.Has("ua") {
		f.UserAgents = q["ua"]
		if len(f.UserAgents) > maxUsageUAs {
			f.UserAgents = f.UserAgents[:maxUsageUAs]
		}
	}
	return f
}

// unixParam parses unix seconds; anything else means "no bound".
func unixParam(v string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

const (
	defaultUsageDays = 371 // 53 weeks, what the calendar heatmap renders
	maxUsageDays     = 3660
	maxUsageUAs      = 200
)

type usageMeta struct {
	Enabled              bool  `json:"enabled"`
	StoreBody            bool  `json:"store_body"`
	RetentionSeconds     int64 `json:"retention_seconds"`
	BodyRetentionSeconds int64 `json:"body_retention_seconds"`
}

func (s *Server) usageMeta() usageMeta {
	meta := usageMeta{Enabled: s.logger != nil && s.logger.Enabled()}
	if cfg := s.currentCfg(); cfg != nil {
		meta.StoreBody = cfg.RequestLog.StoreBody
	}
	// Retention is read from the logger: it is fixed at boot, so the config may
	// already hold a newer value that has not taken effect yet.
	rows, bodies := s.logger.Retention()
	meta.RetentionSeconds = int64(rows.Seconds())
	meta.BodyRetentionSeconds = int64(bodies.Seconds())
	return meta
}

// handleUsage serves the hourly rollup folded into the caller's local days
// (bucket=day) or weekday x hour slots (bucket=weekhour). tz is the caller's
// UTC offset in minutes east, e.g. 480 for UTC+8.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days := defaultUsageDays
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "days must be a positive integer")
			return
		}
		days = min(n, maxUsageDays)
	}
	var offset int64
	if v := q.Get("tz"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < -14*60 || n > 14*60 {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "tz must be a UTC offset in minutes between -840 and 840")
			return
		}
		offset = int64(n) * 60
	}
	bucket := q.Get("bucket")
	if bucket == "" {
		bucket = "day"
	}
	if bucket != "day" && bucket != "weekhour" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "bucket must be day or weekhour")
		return
	}

	now := time.Now()
	from, firstDay, lastDay := reqlog.UsageWindow(now, days, offset)
	buckets, err := s.logger.Usage(r.Context(), from, now.Add(time.Hour))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	var items any
	if bucket == "weekhour" {
		items = reqlog.AggregateWeekHour(buckets, offset)
	} else {
		items = reqlog.AggregateDaily(buckets, offset)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"meta":      s.usageMeta(),
		"bucket":    bucket,
		"first_day": firstDay,
		"last_day":  lastDay,
		"items":     items,
	})
}
