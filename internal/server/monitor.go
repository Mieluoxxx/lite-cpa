package server

import (
	"embed"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
	return reqlog.ListFilter{
		Model:      strings.TrimSpace(q.Get("model")),
		Upstream:   strings.TrimSpace(q.Get("upstream")),
		Protocol:   strings.TrimSpace(q.Get("protocol")),
		Status:     strings.TrimSpace(q.Get("status")),
		Outcome:    strings.TrimSpace(q.Get("outcome")),
		ErrorsOnly: truthy(q.Get("errors")),
	}
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
