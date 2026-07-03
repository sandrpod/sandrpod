// Copyright 2024 SandrPod
// E2B filesystem watch surface (watch_dir). The SDK model is poll-based, on the
// Filesystem connect service:
//
//	CreateWatcher(path, recursive) → {watcherId}
//	GetWatcherEvents(watcherId)    → {events:[{name,type,entry?}]}
//	RemoveWatcher(watcherId)
//
// When the EnvdBackend also implements EnvdWatchBackend, envd serves these by
// proxying to the toolbox /watch/* endpoints; otherwise they return
// unimplemented.

package e2bcompat

import (
	"encoding/json"
	"net/http"
)

// WatchEvent is one filesystem change (E2B FilesystemEvent). Type is the E2B
// EventType enum name (e.g. "EVENT_TYPE_CREATE").
type WatchEvent struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// EnvdWatchBackend is the optional directory-watch surface.
type EnvdWatchBackend interface {
	CreateWatcher(sandboxID, path string, recursive bool) (string, error)
	GetWatcherEvents(sandboxID, watcherID string) ([]WatchEvent, error)
	RemoveWatcher(sandboxID, watcherID string) error
}

func (e *envd) watchBackend() (EnvdWatchBackend, bool) {
	wb, ok := e.backend.(EnvdWatchBackend)
	return wb, ok
}

// fsCreateWatcher handles the CreateWatcher unary.
func (e *envd) fsCreateWatcher(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	wb, has := e.watchBackend()
	if !has {
		writeConnectErrStatus(w, http.StatusNotImplemented, "unimplemented", "watch not supported")
		return
	}
	var path string
	var recursive bool
	if proto {
		path = decodeStringField(body, 1)
		recursive = decodeBoolField(body, 2)
	} else {
		var req struct {
			Path      string `json:"path"`
			Recursive bool   `json:"recursive"`
		}
		_ = json.Unmarshal(body, &req)
		path, recursive = req.Path, req.Recursive
	}
	id, err := wb.CreateWatcher(sandboxOf(r), path, recursive)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, encodeCreateWatcherResponse(id), map[string]any{"watcherId": id})
}

// fsGetWatcherEvents handles the GetWatcherEvents unary.
func (e *envd) fsGetWatcherEvents(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	wb, has := e.watchBackend()
	if !has {
		writeConnectErrStatus(w, http.StatusNotImplemented, "unimplemented", "watch not supported")
		return
	}
	id := watcherIDOf(body, proto)
	events, err := wb.GetWatcherEvents(sandboxOf(r), id)
	if err != nil {
		writeConnectErr(w, err)
		return
	}
	if events == nil {
		events = []WatchEvent{}
	}
	writeUnary(w, proto, encodeWatcherEventsResponse(events), watcherEventsJSON(events))
}

// fsRemoveWatcher handles the RemoveWatcher unary.
func (e *envd) fsRemoveWatcher(w http.ResponseWriter, r *http.Request) {
	body, proto, ok := readUnary(w, r)
	if !ok {
		return
	}
	wb, has := e.watchBackend()
	if !has {
		writeConnectErrStatus(w, http.StatusNotImplemented, "unimplemented", "watch not supported")
		return
	}
	if err := wb.RemoveWatcher(sandboxOf(r), watcherIDOf(body, proto)); err != nil {
		writeConnectErr(w, err)
		return
	}
	writeUnary(w, proto, nil, struct{}{})
}

// watcherIDOf pulls the watcher_id (field 1) out of a request.
func watcherIDOf(body []byte, proto bool) string {
	if proto {
		return decodeStringField(body, 1)
	}
	var req struct {
		WatcherID string `json:"watcherId"`
	}
	_ = json.Unmarshal(body, &req)
	return req.WatcherID
}

// watcherEventsJSON shapes events into the proto3-JSON GetWatcherEventsResponse.
func watcherEventsJSON(events []WatchEvent) map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		out = append(out, map[string]any{"name": ev.Name, "type": ev.Type})
	}
	return map[string]any{"events": out}
}
