package prompt

import (
	_ "embed"
	"strings"
	"sync/atomic"
)

// superModeText is the user-authored Super-mode preamble, embedded verbatim so
// it ships in the single binary and is never re-typed (or re-worded) by code.
// The file is the source of truth: edit it there, not here.
//
//go:embed supermode.md
var superModeText string

// superModeOn is the process-wide Super-mode switch. Config is loaded per turn
// by every front end (serve re-reads config.yml each turn, the CLI/TUI once at
// startup), and Compose is a pure function that must not import config — so
// callers push the resolved value here (see Server.buildAgent,
// Server.runChannelTurns and cmd/octo's chat loop) and Compose reads it.
var superModeOn atomic.Bool

// SetSuperMode turns Super-mode prompt injection on or off process-wide.
// Idempotent and safe for concurrent use.
func SetSuperMode(on bool) { superModeOn.Store(on) }

// SuperModeEnabled reports whether Compose currently prepends superModeText.
func SuperModeEnabled() bool { return superModeOn.Load() }

// SuperModeFirstLine is superModeText's first line — the marker used to tell
// whether a composed prompt was already injected with (see HasSuperMode). It is
// derived from the embedded text rather than duplicated as a literal so the two
// can never drift apart.
func SuperModeFirstLine() string {
	if i := strings.IndexByte(superModeText, '\n'); i >= 0 {
		return superModeText[:i]
	}
	return superModeText
}

// HasSuperMode reports whether s already carries the Super-mode preamble.
// Callers outside this package use it to decide whether a frozen prompt is
// stale with respect to the current switch (see Session.SetComposedSystem's
// freeze in internal/server).
func HasSuperMode(s string) bool { return strings.Contains(s, SuperModeFirstLine()) }

// applySuperMode prepends the Super-mode preamble to a composed system prompt
// when the process-wide switch is on. Callers that resolve the switch per
// project/session use applySuperModeDecision with their own value instead.
func applySuperMode(s string) string {
	return applySuperModeDecision(s, SuperModeEnabled())
}

// applySuperModeDecision is applySuperMode against an explicit decision.
//
// It is a no-op — byte for byte — when on is false, and idempotent when the
// text is already present (HasSuperMode), so a caller that composes twice
// (full + lean, or a re-freeze) can never double-inject.
//
// The separator is the same "\n\n---\n\n" Compose already uses between layers:
// exactly two newlines and a horizontal rule. superModeText ends with a newline
// of its own, so only one more is added — the text itself is never trimmed,
// edited, or reordered.
func applySuperModeDecision(s string, on bool) string {
	if !on || HasSuperMode(s) {
		return s
	}
	prefix := superModeText
	if !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	return prefix + "\n---\n\n" + s
}
