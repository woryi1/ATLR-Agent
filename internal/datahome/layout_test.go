package datahome

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// flatTree builds a miniature copy of the pre-split layout: a few files in each
// future region, one workspace with a task folder, and a session transcript
// that still points at the old location.
func flatTree(t *testing.T, root string) (dataDir, sessionPath string) {
	t.Helper()
	dataDir = filepath.Join(root, dataDirName)
	for _, dir := range []string{
		"sessions", "skills", "themes", "music-cache", "webview", "tmp", "trash",
		filepath.Join("tasks"),
	} {
		if err := os.MkdirAll(filepath.Join(dataDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{
		"config.yml", "desktop.json", "bili-cookies.json", "soul.md", "user.md",
		"session-groups.json", "audit.log", "music-state.json", "serve.log",
	} {
		if err := os.WriteFile(filepath.Join(dataDir, file), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldWork := filepath.Join(root, workspaceFolder, "tasks", "20260101-000000-abcd1234")
	if err := os.MkdirAll(oldWork, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath = filepath.Join(dataDir, "sessions", "20260101-000000-abcd1234.jsonl")
	rec := map[string]any{
		"type":        "meta",
		"cwd":         oldWork,
		"working_dir": oldWork,
		"title":       "old session",
	}
	line, _ := json.Marshal(rec)
	if err := os.WriteFile(sessionPath, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return dataDir, sessionPath
}

func TestMigrationSplitsRegionsAndRewritesSessions(t *testing.T) {
	root := t.TempDir()
	t.Setenv(PortableRootEnv, root)
	t.Setenv(ProfileEnv, "")
	dataDir, _ := flatTree(t, root)

	if err := EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}

	want := map[string][]string{
		RegionUser:    {"config.yml", "desktop.json", "bili-cookies.json", "soul.md", "user.md", "themes"},
		RegionRecords: {"sessions", "session-groups.json", "audit.log"},
		RegionTools:   {"skills"},
		RegionCache:   {"music-cache", "webview", "tmp", "trash", "music-state.json", "serve.log"},
	}
	for region, names := range want {
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(dataDir, region, name)); err != nil {
				t.Errorf("%s/%s missing after migration: %v", region, name, err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
				t.Errorf("%s still at the flat root after migration", name)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, RegionWork, "Tasks", "20260101-000000-abcd1234")); err != nil {
		t.Errorf("task workspace not moved into Work\\Tasks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, workspaceFolder)); err == nil {
		t.Errorf("Workspace folder still exists beside the exe")
	}

	// The transcript must point at the new location, escaped or not.
	session := filepath.Join(dataDir, RegionRecords, "sessions", "20260101-000000-abcd1234.jsonl")
	raw, err := os.ReadFile(session)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dataDir, RegionWork, "Tasks", "20260101-000000-abcd1234")
	if !containsEscaped(string(raw), wantPath) {
		t.Errorf("session working_dir not rewritten to %s:\n%s", wantPath, raw)
	}

	// Path() resolves through the region now.
	p, err := Path("sessions")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dataDir, RegionRecords, "sessions") {
		t.Errorf("Path(sessions) = %s", p)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	root := t.TempDir()
	t.Setenv(PortableRootEnv, root)
	t.Setenv(ProfileEnv, "")
	dataDir, _ := flatTree(t, root)

	if err := EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	if v := layoutVersion(dataDir); v != LayoutVersion {
		t.Errorf("layout version = %d, want %d", v, LayoutVersion)
	}
	if _, err := os.Stat(filepath.Join(dataDir, RegionUser, "config.yml")); err != nil {
		t.Errorf("second run moved data: %v", err)
	}
}

func TestFreshLayoutHasNoLegacyDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv(PortableRootEnv, root)
	t.Setenv(ProfileEnv, "")
	if err := EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, dataDirName)
	for _, region := range requiredRegions {
		if _, err := os.Stat(filepath.Join(dataDir, region)); err != nil {
			t.Errorf("region %s missing on a fresh tree: %v", region, err)
		}
	}
}

func containsEscaped(haystack, path string) bool {
	if filepath.Separator == '\\' {
		esc := ""
		for _, r := range path {
			if r == '\\' {
				esc += `\\`
			} else {
				esc += string(r)
			}
		}
		return contains(haystack, esc) || contains(haystack, path)
	}
	return contains(haystack, path)
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
