package server

// Token-saving skill, forced on every user turn.
//
// The skill itself ships with the binary (internal/skills/defaults/token-saving,
// materialized into the managed defaults root) — this file is only the switch
// and the per-turn nudge. The nudge rides UserPromptSubmit, the same in-process
// hook the memory injector uses, so it lands in front of EVERY user message
// rather than once per session or per project.

import (
	"context"
	"net/http"

	"github.com/open-octo/octo-agent/internal/config"
	"github.com/open-octo/octo-agent/internal/hooks"
)

const tokenSkillReminder = "<system-reminder>\n" +
	"省 Token 规则（对每一句用户消息都生效）：在回应这条消息之前，你必须先调用 skill 工具加载 name=\"token-saving\" 的技能，并按它的工作法执行。即使本会话之前已经加载过、即使消息很短，也要按这条规则重新对照一次。\n" +
	"</system-reminder>"

// tokenSkillEnabled reads the switch fresh each turn, so a flip in Settings
// takes effect on the very next message.
func tokenSkillEnabled() bool {
	cfg, err := config.LoadCached()
	if err != nil {
		return true // unreadable config: keep the default-on behaviour
	}
	return cfg.ForceTokenSkillOn()
}

// registerTokenSkillHook wires the per-turn reminder onto a session's engine.
func registerTokenSkillHook(e *hooks.Engine) {
	if e == nil {
		return
	}
	e.RegisterInProc(hooks.EventUserPromptSubmit, func(_ context.Context, _ hooks.Payload) string {
		if !tokenSkillEnabled() {
			return ""
		}
		return tokenSkillReminder
	})
}

// handleGetForceTokenSkill reports the switch (unset counts as on).
func (s *Server) handleGetForceTokenSkill(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": cfg.ForceTokenSkillOn()})
}

// handlePutForceTokenSkill persists the switch. The hook reads it per turn, so
// no re-freeze is needed.
func (s *Server) handlePutForceTokenSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := readBodyJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	cfg.ForceTokenSkill = &body.Enabled
	if err := cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": body.Enabled})
}
