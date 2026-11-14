package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestFallbackDirIfRootLaunch(t *testing.T) {
	// Real per-platform shapes: fallbackDirIfRootLaunch detects a root as "a
	// path that is its own parent" via filepath.Dir, whose separator handling
	// is platform-specific — POSIX "/" is not a root on Windows ("C:\" is).
	root, exeDir := "/", "/Applications/Octo.app"
	if runtime.GOOS == "windows" {
		root, exeDir = `C:\`, `D:\Portable\octo`
	}
	cases := []struct {
		name string
		wd   string
		fb   string
		want string
	}{
		{"gui launch at filesystem root", root, exeDir, exeDir},
		{"getwd failed (empty)", "", exeDir, exeDir},
		{"terminal launch in a project dir", filepath.Join(exeDir, "Workspace", "proj"), exeDir, ""},
		{"exe dir itself is fine", exeDir, exeDir, ""},
		{"no fallback resolved leaves cwd untouched", root, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fallbackDirIfRootLaunch(tc.wd, tc.fb); got != tc.want {
				t.Errorf("fallbackDirIfRootLaunch(%q, %q) = %q, want %q", tc.wd, tc.fb, got, tc.want)
			}
		})
	}
}
