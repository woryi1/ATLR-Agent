package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/open-octo/octo-agent/internal/datahome"
)

// desktopSettings holds the desktop app's per-machine preferences — the ones
// the server itself has no opinion about. Persisted to the profile-scoped Octo
// data root's desktop.json so they survive a relaunch.
type desktopSettings struct {
	// window is closed, hiding to the tray instead of quitting. Default true —
	// closing the window shouldn't drop a VS Code / phone client's backend.
	KeepRunningInBackground bool `json:"keep_running_in_background"`
	// SeededOctoVersion records the version of the octo CLI this app last seeded
	// to ~/.local/bin (macOS + Linux; Windows ships the CLI through its
	// installer). It scopes the refresh-on-upgrade: an octo on ~/.local/bin
	// with no matching record here is the user's own and is never overwritten.
	SeededOctoVersion string `json:"seeded_octo_version,omitempty"`
	// RememberWindowSize opts into restoring the last windowed size on the next
	// launch. Off by default: a remembered size is what opened the app at an old
	// aspect ratio after moving to a different display.
	RememberWindowSize bool `json:"remember_window_size,omitempty"`
	// WindowWidth/WindowHeight hold that remembered size, and are only written
	// while RememberWindowSize is on and the window is in its normal state.
	WindowWidth  int `json:"window_width,omitempty"`
	WindowHeight int `json:"window_height,omitempty"`
	// PetX/PetY remember the pet window's position across relaunches, in
	// absolute screen points. PetPositionSet is the "user has placed the pet"
	// flag: (0,0) is a legitimate on-screen position, so the coordinates alone
	// can't distinguish a saved position from a fresh install's zero value.
	PetX           int  `json:"pet_x,omitempty"`
	PetY           int  `json:"pet_y,omitempty"`
	PetPositionSet bool `json:"pet_position_set,omitempty"`
}

// defaultDesktopSettings is what a first launch (no file yet) uses.
func defaultDesktopSettings() desktopSettings {
	return desktopSettings{KeepRunningInBackground: true}
}

func desktopSettingsPath() (string, error) {
	path, err := datahome.Path("desktop.json")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// loadDesktopSettings reads the profile-scoped desktop.json, falling back to defaults for
// a missing or unreadable file (a fresh install, or a hand-corrupted one — the
// defaults are safe either way).
func loadDesktopSettings() desktopSettings {
	s := defaultDesktopSettings()
	path, err := desktopSettingsPath()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s) // partial/corrupt JSON keeps the defaults it couldn't override
	return s
}

// saveDesktopSettings writes the settings back to the profile-scoped desktop.json.
func saveDesktopSettings(s desktopSettings) error {
	path, err := desktopSettingsPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
