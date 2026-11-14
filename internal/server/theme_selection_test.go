package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGetThemeSelection_FreshInstall: with no selection.json on disk the GET
// must answer with an explicit "configured": false. The omitempty that used to
// be on the field serialized a fresh install as "{}", the client's strict
// configured check could never match, and the very first launch played the
// boot splash — which the boot-splash contract says must not happen (the
// splash is a "welcome back", not a welcome).
func TestGetThemeSelection_FreshInstall(t *testing.T) {
	themeHome(t)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	w := doJSON(t, srv, "GET", "/api/theme-selection", "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	v, ok := out["configured"]
	if !ok {
		t.Fatalf("response omits \"configured\": %s", w.Body.String())
	}
	if b, isBool := v.(bool); !isBool || b {
		t.Errorf("expected configured=false, got %v", v)
	}
}

// TestGetThemeSelection_PersistedRoundTrip: a PUT persists the choice and the
// GET carries it back, with startupAnim surviving as an explicit false (the
// "user turned the splash off" state must not collapse into "never set").
func TestGetThemeSelection_PersistedRoundTrip(t *testing.T) {
	themeHome(t)
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	body := `{"mode":"dark","pack":"blossom","background":"bloom","startupAnim":false,"backgroundOpacity":0.35}`
	w := doJSON(t, srv, "PUT", "/api/theme-selection", body)
	if w.Code != 200 {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = doJSON(t, srv, "GET", "/api/theme-selection", "")
	if w.Code != 200 {
		t.Fatalf("GET expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"startupAnim":false`) {
		t.Errorf("startupAnim=false not persisted: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"configured"`) {
		t.Errorf("configured key missing: %s", w.Body.String())
	}
	var out themeSelection
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mode != "dark" || out.Pack != "blossom" || out.Background != "bloom" {
		t.Errorf("selection lost: %+v", out)
	}
	if out.BackgroundOpacity == nil || *out.BackgroundOpacity != 0.35 {
		t.Errorf("opacity lost: %+v", out.BackgroundOpacity)
	}
	if out.StartupAnim == nil || *out.StartupAnim {
		t.Errorf("startupAnim must survive as explicit false: %+v", out.StartupAnim)
	}
}
