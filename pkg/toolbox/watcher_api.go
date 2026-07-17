// Copyright 2026 SandrPod Contributors
// HTTP surface for the filesystem watcher and resource metrics. The e2bcompat
// gateway proxies the E2B watch_dir (CreateWatcher/GetWatcherEvents/
// RemoveWatcher) and get_metrics surfaces onto these:
//
//	POST /watch/create  {path,recursive} → {watcher_id}
//	GET  /watch/events?id=…             → {events:[{name,type}]}
//	POST /watch/remove  {watcher_id}
//	GET  /metrics                        → Metrics

package toolbox

import (
	"encoding/json"
	"net/http"
)

func (s *Server) watchCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	id, err := s.watchManager().Create(req.Path, req.Recursive)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"watcher_id": id})
}

func (s *Server) watchEventsHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	events, ok := s.watchManager().Events(id)
	if !ok {
		http.Error(w, "watcher not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": events})
}

func (s *Server) watchRemoveHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WatcherID string `json:"watcher_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !s.watchManager().Remove(req.WatcherID) {
		http.Error(w, "watcher not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CollectMetrics())
}
