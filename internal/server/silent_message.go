package server

import (
	"net/http"
	"strings"
)

// silentMessageRequest is the body of POST /api/sessions/{id}/silent-message.
type silentMessageRequest struct {
	Text string `json:"text"`
}

// handleSilentMessage serves POST /api/sessions/{id}/silent-message — the web
// counterpart of "type something while the agent is working, but don't
// interrupt it".
//
// The text is queued on the RUNNING turn's Inbox as a Silent item. The agent
// loop drains it at the next iteration boundary — i.e. right after the tool
// batch's results have been written to history and right before the next
// provider call — and appends it as a user message prefixed with
// "[用户补充·静默] ". Nothing else happens: no interrupt, no new turn, no
// extra LLM round, no turn-count change, and no history_user_message echo (the
// note is not user speech, so the transcript gets no bubble for it).
//
// 409 when no turn is running: there is no iteration boundary to attach to, so
// the caller must fall back to a normal send. We never do that fallback
// ourselves — silently turning an "append" into a "new turn" is exactly the
// behaviour the user asked to avoid, so the decision is theirs.
func (s *Server) handleSilentMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}
	var req silentMessageRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	// Enqueue under sessionAgentsMu — the same lock every other mid-turn
	// writer uses (bg_tasks.go deliverModelNote, handleWSUserMessage). Holding
	// it across the Enqueue is what makes "agent found" mean "note delivered":
	// the turn's teardown drains and deregisters under this same lock, so a
	// note can never slip into an Inbox that is about to be discarded.
	s.sessionAgentsMu.Lock()
	a := s.sessionAgents[sessionID]
	if a == nil {
		s.sessionAgentsMu.Unlock()
		writeError(w, http.StatusConflict, "no turn is running for this session")
		return
	}
	a.Inbox.EnqueueSilent(text)
	pending := a.Inbox.Pending()
	s.sessionAgentsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"queued": true, "pending": pending})
}
