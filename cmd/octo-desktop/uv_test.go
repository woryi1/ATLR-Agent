package main

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/open-octo/octo-agent/internal/datahome"
)

func TestBundledUvTargetUsesSharedBinDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(datahome.PortableRootEnv, home)
	name := "uv"
	if runtime.GOOS == "windows" {
		name = "uv.exe"
	}
	for _, tc := range []struct {
		name    string
		profile string
	}{
		{"default profile", ""},
		{"work profile", "work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(datahome.ProfileEnv, tc.profile)
			got, err := bundledUvTarget()
			if err != nil {
				t.Fatal(err)
			}
			// Helper binaries are machine-scoped: every profile shares the
			// default tree's Tools\bin.
			want := filepath.Join(home, "Data", datahome.RegionTools, "bin", name)
			if got != want {
				t.Errorf("bundled uv target = %q, want %q", got, want)
			}
		})
	}
}
