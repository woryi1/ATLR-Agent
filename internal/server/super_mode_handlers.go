package server

import (
	"net/http"
	"strings"

	"github.com/open-octo/octo-agent/internal/config"
	"github.com/open-octo/octo-agent/internal/prompt"
)

// ─── GET/PUT /api/super-mode ────────────────────────────────────────────────
//
// Super mode is a PROJECT-level switch, stored as `super_mode` on the project
// record (session_groups.go) — every project remembers its own value and a new
// project starts with it off (the field's zero value). A session that belongs
// to no project still resolves against the global config.yml `super_mode`, so
// loose tasks and old clients keep the previous behaviour.
//
// When on, every system prompt composed for a session in that project gets the
// fixed preamble in internal/prompt/supermode.md prepended (see
// prompt.compose/applySuperModeDecision). The server resolves the value per
// turn from the session's project in buildAgent / runChannelTurns and passes it
// explicitly — the prompt package's process-wide switch stays for the CLI/TUI,
// which has no projects.
//
// Effect timing. The preamble is baked into a session's composed system prompt,
// which is frozen on that session's first turn (Session.SetComposedSystem) so
// the provider's prompt-cache prefix survives turn over turn. A flip therefore
// cannot reach an already-composed string — but it does not need a new session
// either: buildAgent / runChannelTurns compare HasSuperMode(frozen) against the
// freshly resolved project value each turn and re-freeze (ClearComposedSystem)
// when they disagree, so the change lands on that session's NEXT turn, at the
// cost of one prompt-cache miss for it. Brand-new sessions get it on their
// first turn.

// superModeState is the wire shape for both verbs: {"enabled": bool}, plus
// "scope" saying which store answered — the project's own record or the global
// config fallback. Only clients that care can read it; the bolt reads enabled.
type superModeState struct {
	Enabled bool   `json:"enabled"`
	Scope   string `json:"scope,omitempty"`
}

// superModePutRequest is PUT's body. Group may also arrive as ?group=; the
// query parameter wins so a client that got the id from the URL keeps the body
// to just {"enabled": …}.
type superModePutRequest struct {
	Enabled bool   `json:"enabled"`
	Group   string `json:"group,omitempty"`
}

// superModeGroupID picks the project this request is about: ?session= first
// (the session's OWN project is the one a turn composes against — the view's
// project id can name a different project), then ?group=, then the body.
func superModeGroupID(r *http.Request, bodyGroup string) string {
	if sid := strings.TrimSpace(r.URL.Query().Get("session")); sid != "" {
		if p := projectForSession(sid); p != nil {
			return p.ID
		}
	}
	if g := strings.TrimSpace(r.URL.Query().Get("group")); g != "" {
		return g
	}
	return strings.TrimSpace(bodyGroup)
}

// handleGetSuperMode reports the switch that applies to the given project
// context. Read straight off config.Load (not LoadCached): this is a settings
// surface, so a hand-edited config.yml should show its actual current value
// rather than the last one that parsed — same choice the other settings
// handlers make. When ?group/?session names a real project its own value is the
// answer; otherwise the global value is, exactly as before this field existed.
func (s *Server) handleGetSuperMode(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	if gid := superModeGroupID(r, ""); gid != "" {
		if on, ok := projectSuperMode(gid); ok {
			writeJSON(w, http.StatusOK, superModeState{Enabled: on, Scope: "project"})
			return
		}
	}
	writeJSON(w, http.StatusOK, superModeState{Enabled: cfg.SuperMode, Scope: "global"})
}

// handlePutSuperMode persists the switch. With a project context it writes the
// project's record and leaves config.yml alone; without one it writes the
// global value and applies it process-wide immediately, so sessions with no
// project that take their first turn after this response already see it
// (existing sessions re-freeze on their next turn — see the package comment
// above).
func (s *Server) handlePutSuperMode(w http.ResponseWriter, r *http.Request) {
	var body superModePutRequest
	if err := readBodyJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if gid := superModeGroupID(r, body.Group); gid != "" {
		ok, err := setProjectSuperMode(gid, body.Enabled)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save project settings")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeJSON(w, http.StatusOK, superModeState{Enabled: body.Enabled, Scope: "project"})
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	cfg.SuperMode = body.Enabled
	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save config")
		return
	}

	// Apply before responding: a turn that starts the instant the client sees
	// the new state must compose against it.
	prompt.SetSuperMode(cfg.SuperMode)

	writeJSON(w, http.StatusOK, superModeState{Enabled: cfg.SuperMode, Scope: "global"})
}
