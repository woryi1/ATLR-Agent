package server

// Debug log intake. The frontend posts playback events here so the page and the
// Go proxy land in the same file (Data/Cache/video-debug.log), which is what
// makes one playback session readable end to end.

import (
	"net/http"

	"github.com/open-octo/octo-agent/internal/music"
)

type debugLogRequest struct {
	Event  string         `json:"event"`
	Detail map[string]any `json:"detail"`
}

// POST /api/debug-log — append one frontend event to the shared video debug log.
// Local-only: this endpoint exists for the desktop shell's diagnostics.
func (s *Server) handleDebugLog(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeError(w, http.StatusForbidden, "available only from the local machine")
		return
	}
	var req debugLogRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}
	if req.Event == "" {
		writeError(w, http.StatusBadRequest, "missing event")
		return
	}
	music.LogDebug("fe", req.Event, req.Detail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
