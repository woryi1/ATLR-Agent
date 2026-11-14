package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

// A registry written before super_mode existed decodes to "never set" (nil,
// not an error), and the field round-trips through save/load.
func TestProjectSuperMode_LegacyZeroAndRoundTrip(t *testing.T) {
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
	if len(groups) != 1 {
		t.Fatalf("legacy groups = %+v", groups)
	}
	if groups[0].SuperMode != nil {
		t.Fatalf("legacy group super_mode = %v, want nil (inherit the global switch)", *groups[0].SuperMode)
	}

	groups[0].SuperMode = boolPtr(true)
	if err := saveSessionGroups(groups); err != nil {
		t.Fatal(err)
	}
	got := findGroupByID(t, "g1")
	if got.SuperMode == nil || !*got.SuperMode {
		t.Fatal("super_mode did not survive a round trip")
	}
}

// superModeForProject is the per-turn precedence:
//   - no project, or a project that never chose → the global config value
//   - a project that chose on/off → its own value, outranking the global one
//
// The middle case is the one that used to be broken: a plain bool could not
// tell "never set" from "explicitly off", so every project silently outranked
// the global switch and a global toggle could never reach a session that
// belonged to a project.
func TestSuperModeForProject_Precedence(t *testing.T) {
	if got := superModeForProject(nil, true); !got {
		t.Error("no project must fall back to the global value (true)")
	}
	if got := superModeForProject(nil, false); got {
		t.Error("no project must fall back to the global value (false)")
	}
	if got := superModeForProject(&sessionGroup{SuperMode: nil}, true); !got {
		t.Error("a project that never chose must inherit the global value (true)")
	}
	if got := superModeForProject(&sessionGroup{SuperMode: nil}, false); got {
		t.Error("a project that never chose must inherit the global value (false)")
	}
	if got := superModeForProject(&sessionGroup{SuperMode: boolPtr(true)}, false); !got {
		t.Error("project on must outrank global off")
	}
	if got := superModeForProject(&sessionGroup{SuperMode: boolPtr(false)}, true); got {
		t.Error("project off must outrank global on")
	}
}

// The HTTP surface: a project that never chose follows the global switch; an
// explicit choice on the project record outranks it; PUT persists on the
// project's own record (not config.yml); the value survives a cache reset
// ("restart"); a session context resolves its OWN project.
func TestProjectSuperMode_APIDefaultPutPersistAndIsolation(t *testing.T) {
	isolatedPortableRoot(t)
	srv := groupTestServer(t)
	gidA, _ := newProjectGroupWS(t, srv, "Alpha", t.TempDir())
	gidB, _ := newProjectGroupWS(t, srv, "Beta", t.TempDir())
	sidA := createSessionInGroupViaAPI(t, srv, gidA)

	get := func(target string) map[string]any {
		t.Helper()
		rec, out := doGroupReq(t, srv, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
		}
		return out
	}

	// ① A brand-new project has made no choice: it follows the global switch,
	// which starts off.
	out := get("/api/super-mode?group=" + gidA)
	if out["enabled"] != false {
		t.Fatalf("new project = %v, want enabled:false (global default)", out)
	}

	// The global switch is turned on; a project that never chose now follows
	// it — this is the inheritance the old bool field could not express.
	rec, out := doGroupReq(t, srv, http.MethodPut, "/api/super-mode", map[string]any{"enabled": true})
	if rec.Code != http.StatusOK || out["scope"] != "global" || out["enabled"] != true {
		t.Fatalf("PUT global = %d %v", rec.Code, out)
	}
	if out := get("/api/super-mode?group=" + gidA); out["enabled"] != true {
		t.Fatalf("global on did not reach the project that never chose: %v", out)
	}

	// ② PUT true on A lands on A's record and becomes A's own choice.
	rec, out = doGroupReq(t, srv, http.MethodPut, "/api/super-mode?group="+gidA, map[string]any{"enabled": true})
	if rec.Code != http.StatusOK || out["enabled"] != true || out["scope"] != "project" {
		t.Fatalf("PUT project = %d %v", rec.Code, out)
	}
	if g := findGroupByID(t, gidA); g.SuperMode == nil || !*g.SuperMode {
		t.Fatal("project A record not updated")
	}
	// ③ B never chose, so it still follows the global value — and stays
	// independent of A's explicit choice.
	if g := findGroupByID(t, gidB); g.SuperMode != nil {
		t.Fatal("project B picked up project A's explicit value")
	}
	// Restart: drop the in-process registry cache, read the file again.
	groupMu.Lock()
	invalidateRegistryCache()
	groupMu.Unlock()
	if out := get("/api/super-mode?group=" + gidA); out["enabled"] != true {
		t.Fatalf("after restart project A = %v, want true", out)
	}
	raw, err := os.ReadFile(mustRegistryPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"super_mode": true`) {
		t.Errorf("registry file lacks super_mode: %s", raw)
	}

	// ④ No project context falls back to the global value (true above).
	if out := get("/api/super-mode"); out["enabled"] != true || out["scope"] != "global" {
		t.Fatalf("no group = %v, want global true", out)
	}
	// A session id resolves its owning project, same as ?group=.
	if out := get("/api/super-mode?session=" + sidA); out["enabled"] != true || out["scope"] != "project" {
		t.Fatalf("session context = %v, want project true", out)
	}
	// An unknown group falls back to global rather than 404ing the read.
	if out := get("/api/super-mode?group=missing"); out["scope"] != "global" {
		t.Fatalf("unknown group = %v, want global fallback", out)
	}

	// PATCH /api/session-groups/{id} carries super_mode too (pointer: explicit
	// false must be accepted, not treated as "omitted"). A project that
	// explicitly turns it off outranks the global switch.
	rec, _ = doGroupReq(t, srv, http.MethodPatch, "/api/session-groups/"+gidA, map[string]any{"super_mode": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH super_mode = %d: %s", rec.Code, rec.Body.String())
	}
	if g := findGroupByID(t, gidA); g.SuperMode == nil || *g.SuperMode {
		t.Error("PATCH false did not turn project A off")
	}
	if out := get("/api/super-mode?group=" + gidA); out["enabled"] != false || out["scope"] != "project" {
		t.Fatalf("explicit off = %v, want enabled:false scope:project", out)
	}
	// PUT against a missing group is a 404, not a silent success.
	rec, _ = doGroupReq(t, srv, http.MethodPut, "/api/super-mode?group=missing", map[string]any{"enabled": true})
	if rec.Code != http.StatusNotFound {
		t.Errorf("PUT unknown group = %d, want 404", rec.Code)
	}
}
