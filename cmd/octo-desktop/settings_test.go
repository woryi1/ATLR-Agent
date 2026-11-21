package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-octo/octo-agent/internal/datahome"
)

// TestSelectDesktopProfile covers the desktop shell's only CLI option. The
// "unknown argument" case is the interesting one: it must NOT fail, because
// this runs before setupCrashLog and a GUI launch has no stderr to explain
// itself on.
func TestSelectDesktopProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		profile string
		wantErr bool
	}{
		{"profile only", []string{"--profile", "work"}, "work", false},
		{"equals profile", []string{"--profile=work"}, "work", false},
		{"missing profile", []string{"--profile="}, "", true},
		{"invalid profile", []string{"--profile", "../escape"}, "", true},
		{"unknown argument is ignored", []string{"-psn_0_12345"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(datahome.ProfileEnv, "")
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv(datahome.PortableRootEnv, home)
			err := selectDesktopProfile(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("selectDesktopProfile(%q) error = %v, want error %v", tc.args, err, tc.wantErr)
			}
			if got := os.Getenv(datahome.ProfileEnv); got != tc.profile {
				t.Errorf("profile = %q, want %q", got, tc.profile)
			}
		})
	}
}

// TestDesktopSettings_PetPositionRoundTrip guards the on-disk contract for the
// pet position. The window size is deliberately not persisted any more — every
// launch sizes itself to the current screen.
func TestDesktopSettings_PetPositionRoundTrip(t *testing.T) {
	in := desktopSettings{
		PetX:           42,
		PetY:           900,
		PetPositionSet: true,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out desktopSettings
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.PetPositionSet || out.PetX != in.PetX || out.PetY != in.PetY {
		t.Errorf("pet position not preserved: got (%d,%d,set=%v), want (%d,%d,set=true)",
			out.PetX, out.PetY, out.PetPositionSet, in.PetX, in.PetY)
	}
}

// TestDefaultDesktopSettings_NoPetPosition documents that a fresh install carries
// no pet position, which showPet reads as "use the bottom-right default".
func TestDefaultDesktopSettings_NoPetPosition(t *testing.T) {
	s := defaultDesktopSettings()
	if s.PetPositionSet || s.PetX != 0 || s.PetY != 0 {
		t.Errorf("defaults should carry no pet position, got %+v", s)
	}
}

func TestDesktopSettingsPathUsesProfileDataHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(datahome.PortableRootEnv, home)
	for _, tc := range []struct {
		name    string
		profile string
	}{
		{"default profile", ""},
		{"work profile", "work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(datahome.ProfileEnv, tc.profile)
			got, err := desktopSettingsPath()
			if err != nil {
				t.Fatal(err)
			}
			root, err := datahome.DirFor(tc.profile)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(root, datahome.RegionUser, "desktop.json")
			if got != want {
				t.Errorf("desktop settings path = %q, want %q", got, want)
			}
		})
	}
}
