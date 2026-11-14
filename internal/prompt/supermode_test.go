package prompt

import (
	"strings"
	"testing"
)

// withSuperMode turns the switch on/off for one test and restores it
// afterwards, so the process-wide flag can't leak between cases.
func withSuperMode(t *testing.T, on bool) {
	t.Helper()
	prev := SuperModeEnabled()
	SetSuperMode(on)
	t.Cleanup(func() { SetSuperMode(prev) })
}

// TestSuperMode_Injected: with the switch on, the embedded preamble leads every
// composed prompt — full and lean alike, since lite-model turns that missed it
// would silently run without the user's rules.
func TestSuperMode_Injected(t *testing.T) {
	dir := t.TempDir()
	withSuperMode(t, true)

	full, lean := ComposePair("USER_RULE", dir, "ENV_BLOCK", "SKILLS_MANIFEST", "MCP_MANIFEST", "MEMORY_BLOCK", false, false)
	for name, got := range map[string]string{"full": full, "lean": lean} {
		if !strings.HasPrefix(got, SuperModeFirstLine()) {
			t.Errorf("%s prompt must START with the super-mode preamble, got:\n%.120s", name, got)
		}
		// Exactly two newlines and a rule separate it from the composed prompt.
		if !strings.Contains(got, "。\n\n---\n\n") {
			t.Errorf("%s prompt: missing the blank line + horizontal rule separator:\n%.200s", name, got)
		}
		// The preamble is used verbatim: its last line survives intact.
		if !strings.Contains(got, strings.TrimRight(superModeText, "\n")) {
			t.Errorf("%s prompt: embedded preamble was altered", name)
		}
	}
}

// TestSuperMode_OffIsByteIdentical: with the switch off, Compose produces
// exactly what it produced before super mode existed — and turning it on only
// prepends, never rewrites, that same string.
func TestSuperMode_OffIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	compose := func() string {
		return Compose("USER_RULE", dir, "ENV_BLOCK", "SKILLS_MANIFEST", "MCP_MANIFEST", "MEMORY_BLOCK", false, false)
	}

	withSuperMode(t, false)
	off := compose()
	if HasSuperMode(off) {
		t.Fatalf("switch off, yet the prompt contains the preamble:\n%.200s", off)
	}
	if again := compose(); again != off {
		t.Error("switch off must be deterministic, got two different prompts")
	}

	withSuperMode(t, true)
	on := compose()
	prefix := superModeText
	if !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	prefix += "\n---\n\n"
	if on != prefix+off {
		t.Errorf("switch on must equal preamble + the exact off-prompt\n got: %.160q\nwant: %.160q", on, prefix+off)
	}
}

// TestSuperMode_Idempotent: composing an already-injected prompt (the re-freeze
// path feeds a previous composition back in as the user layer) must not stack a
// second copy of the preamble.
func TestSuperMode_Idempotent(t *testing.T) {
	dir := t.TempDir()
	withSuperMode(t, true)

	once := Compose("USER_RULE", dir, "ENV_BLOCK", "", "", "", false, false)
	twice := Compose(once, dir, "ENV_BLOCK", "", "", "", false, false)
	if n := strings.Count(twice, SuperModeFirstLine()); n != 1 {
		t.Errorf("preamble injected %d times, want exactly 1:\n%.200s", n, twice)
	}
}

// TestSuperMode_PerCallDecision: the per-project compose path (the server's
// buildAgent/runChannelTurns) must follow its explicit value in BOTH directions,
// regardless of the process-wide switch the CLI/TUI still uses — and stay
// idempotent when the previous composition is fed back in (the re-freeze path).
func TestSuperMode_PerCallDecision(t *testing.T) {
	dir := t.TempDir()
	compose := func(on bool, user string) string {
		return ComposeWithSuperMode(on, user, dir, "ENV_BLOCK", "", "", "", false, false)
	}

	// Global off, project on: injected.
	withSuperMode(t, false)
	on := compose(true, "USER_RULE")
	if !strings.HasPrefix(on, SuperModeFirstLine()) {
		t.Fatalf("per-call true must inject even with the global switch off:\n%.120s", on)
	}
	// Global on, project off: not injected. The project's explicit value wins.
	withSuperMode(t, true)
	off := compose(false, "USER_RULE")
	if HasSuperMode(off) {
		t.Fatalf("per-call false must suppress injection even with the global switch on:\n%.120s", off)
	}
	// Idempotent on the re-freeze path: composing an already-injected prompt.
	twice := compose(true, on)
	if n := strings.Count(twice, SuperModeFirstLine()); n != 1 {
		t.Errorf("preamble injected %d times, want exactly 1", n)
	}
	// Both variants of the pair carry the same decision.
	full, lean := ComposePairWithSuperMode(true, "USER_RULE", dir, "ENV_BLOCK", "SKILLS_MANIFEST", "MCP_MANIFEST", "MEMORY_BLOCK", false, false)
	for name, got := range map[string]string{"full": full, "lean": lean} {
		if !strings.HasPrefix(got, SuperModeFirstLine()) {
			t.Errorf("%s prompt must start with the preamble", name)
		}
	}
}

// TestSuperMode_FirstLineMatchesFile pins the idempotency marker to the file's
// own first line, so a future edit to supermode.md can't leave HasSuperMode
// matching a line that is no longer there.
func TestSuperMode_FirstLineMatchesFile(t *testing.T) {
	line := strings.SplitN(superModeText, "\n", 2)[0]
	if SuperModeFirstLine() != line || strings.TrimSpace(line) == "" {
		t.Errorf("SuperModeFirstLine() = %q, want first line of supermode.md = %q", SuperModeFirstLine(), line)
	}
	if !HasSuperMode(superModeText) {
		t.Error("HasSuperMode must recognise the embedded text itself")
	}
}
