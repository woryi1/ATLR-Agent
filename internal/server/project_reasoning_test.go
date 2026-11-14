package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-octo/octo-agent/internal/agent"
)

// isolatedPortableRoot keeps the registry/config writes of this file inside a
// temp tree. The package's TestMain isolates HOME, but the registry lives under
// the portable data root, whose default is the test binary's directory.
func isolatedPortableRoot(t *testing.T) {
	t.Helper()
	t.Setenv("OCTO_PORTABLE_ROOT", t.TempDir())
}

func findGroupByID(t *testing.T, id string) sessionGroup {
	t.Helper()
	groups, err := loadSessionGroups()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	for _, g := range groups {
		if g.ID == id {
			return g
		}
	}
	t.Fatalf("group %q not found in %+v", id, groups)
	return sessionGroup{}
}

// A registry written before these fields existed must decode to the zero
// value, and a round trip must preserve them.
func TestProjectReasoningEffort_LegacyZeroAndRoundTrip(t *testing.T) {
	isolatedPortableRoot(t)
	path, err := sessionGroupsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"groups":[{"id":"g1","name":"old","session_ids":["s1"],"working_dir":"/tmp/old"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	groups, err := loadSessionGroups()
	if err != nil {
		t.Fatalf("legacy registry must still load: %v", err)
	}
	if len(groups) != 1 || groups[0].ReasoningEffort != "" || groups[0].LastSessionID != "" {
		t.Fatalf("legacy group = %+v, want zero-valued new fields", groups)
	}

	groups[0].ReasoningEffort = "high"
	groups[0].LastSessionID = "s1"
	if err := saveSessionGroups(groups); err != nil {
		t.Fatal(err)
	}
	reloaded := findGroupByID(t, "g1")
	if reloaded.ReasoningEffort != "high" || reloaded.LastSessionID != "s1" {
		t.Fatalf("round trip = %+v, want high/s1", reloaded)
	}
}

// New-session default: the project's saved level outranks the global default,
// which outranks off. An explicit project "off" also outranks the global.
func TestInitialReasoningEffort_Precedence(t *testing.T) {
	cases := []struct{ project, global, want string }{
		{"high", "low", "high"},
		{"", "medium", "medium"},
		{"", "", "off"},
		{"off", "max", "off"},
	}
	for _, c := range cases {
		if got := initialReasoningEffort(c.project, c.global); got != c.want {
			t.Errorf("initialReasoningEffort(%q, %q) = %q, want %q", c.project, c.global, got, c.want)
		}
	}
}

// The write path the slider drives: PATCH persists on the session AND on its
// project, a session born later in that project starts on the saved level, and
// a registry reload (the "restart the server" case) still carries it.
func TestProjectReasoningEffort_PatchPersistsAndSeedsNewSession(t *testing.T) {
	isolatedPortableRoot(t)
	srv := groupTestServer(t)
	gid, _ := newProjectGroupWS(t, srv, "Work", t.TempDir())

	first := createSessionInGroupViaAPI(t, srv, gid)
	rec, _ := doGroupReq(t, srv, http.MethodPatch, "/api/sessions/"+first+"/reasoning_effort",
		map[string]any{"reasoning_effort": "high"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH reasoning_effort = %d: %s", rec.Code, rec.Body.String())
	}

	g := findGroupByID(t, gid)
	if g.ReasoningEffort != "high" {
		t.Errorf("project reasoning_effort = %q, want high", g.ReasoningEffort)
	}
	if g.LastSessionID != first {
		t.Errorf("project last_session_id = %q, want %q", g.LastSessionID, first)
	}
	sess, err := agent.LoadSession(first)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ReasoningEffort != "high" {
		t.Errorf("session reasoning_effort = %q, want high", sess.ReasoningEffort)
	}

	second := createSessionInGroupViaAPI(t, srv, gid)
	seeded, err := agent.LoadSession(second)
	if err != nil {
		t.Fatal(err)
	}
	if seeded.ReasoningEffort != "high" {
		t.Errorf("new session started on %q, want the project's high", seeded.ReasoningEffort)
	}

	// Restart: forget the in-process cache and read the file again.
	groupMu.Lock()
	invalidateRegistryCache()
	groupMu.Unlock()
	again := findGroupByID(t, gid)
	if again.ReasoningEffort != "high" {
		t.Errorf("after cache reset reasoning_effort = %q, want high", again.ReasoningEffort)
	}
	raw, err := os.ReadFile(mustRegistryPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reasoning_effort": "high"`) {
		t.Errorf("registry file lacks reasoning_effort: %s", raw)
	}
	t.Logf("registry after PATCH + new session:\n%s", raw)
	t.Logf("new session %s reasoning_effort = %q (after cache reset: %q)", second, seeded.ReasoningEffort, again.ReasoningEffort)
}

// last_session_id: filing a session records it, an explicit PATCH moves it
// back, and a value naming a non-member is rejected.
func TestProjectLastSession_RecordedAndValidated(t *testing.T) {
	isolatedPortableRoot(t)
	srv := groupTestServer(t)
	gid, _ := newProjectGroupWS(t, srv, "Work", t.TempDir())

	first := createSessionInGroupViaAPI(t, srv, gid)
	second := createSessionInGroupViaAPI(t, srv, gid)
	if got := findGroupByID(t, gid).LastSessionID; got != second {
		t.Fatalf("last_session_id after create = %q, want %q", got, second)
	}

	rec, _ := doGroupReq(t, srv, http.MethodPatch, "/api/session-groups/"+gid,
		map[string]any{"last_session_id": first})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH last_session_id = %d: %s", rec.Code, rec.Body.String())
	}
	if got := findGroupByID(t, gid).LastSessionID; got != first {
		t.Errorf("last_session_id = %q, want %q", got, first)
	}

	rec, _ = doGroupReq(t, srv, http.MethodPatch, "/api/session-groups/"+gid,
		map[string]any{"last_session_id": "not-a-member"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH foreign last_session_id = %d, want 400", rec.Code)
	}
	if got := findGroupByID(t, gid).LastSessionID; got != first {
		t.Errorf("rejected write changed last_session_id to %q", got)
	}
}

func mustRegistryPath(t *testing.T) string {
	t.Helper()
	path, err := sessionGroupsPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}
