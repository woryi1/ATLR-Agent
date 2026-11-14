package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// ─── Theme selection ────────────────────────────────────────────────────────
//
// The SELECTED theme mode + pack used to live only in the webview's
// localStorage, which Wails v3 pins under %APPDATA% per machine — migrating
// the Data directory to another computer carried the theme files but not the
// choice. The selection now persists in Data/themes/selection.json so it
// travels with the portable directory; the web UI treats it as the source of
// truth and keeps localStorage only as an offline fallback.

// themeSelection is the persisted theme picker state.
type themeSelection struct {
	// Mode is "" | "light" | "dark" | "system".
	Mode string `json:"mode,omitempty"`
	// Pack is the theme pack id ("" = the built-in default).
	Pack string `json:"pack,omitempty"`
	// Background is the wallpaper id: "" | "off" (no image), "bloom"/"mystery"
	// (built-in), or "custom" (uploaded). "" and "off" render the plain app.
	Background string `json:"background,omitempty"`
	// StartupAnim gates the boot splash (dsh-startup-screen). A pointer keeps
	// "never set" (nil → the client defaults it on) distinct from an explicit
	// false, so a fresh install plays the animation until the user turns it
	// off, and the choice travels with the portable Data directory.
	StartupAnim *bool `json:"startupAnim,omitempty"`
	// BackgroundFit is how the wallpaper fills the viewport: "cover" (default,
	// fill and crop), "contain" (whole image, letterboxed) or "fill" (stretch).
	// Only meaningful when a background is set.
	BackgroundFit string `json:"backgroundFit,omitempty"`
	// BackgroundOpacity is the wallpaper layer's opacity, 0.05–0.8; nil keeps
	// the default 0.2. BackgroundBlur blurs it, 0–30px; nil keeps 0.
	BackgroundOpacity *float64 `json:"backgroundOpacity,omitempty"`
	BackgroundBlur    *float64 `json:"backgroundBlur,omitempty"`
	// Configured is response-only (PUT ignores it): whether onboarding has
	// been completed or skipped. It is a statement about the onboarding state,
	// NOT a playback switch — the client plays the boot splash on every launch
	// and uses this only to pick the shorter, non-interactive first-run pass.
	// (The splash toggle itself is StartupAnim above, i.e. PUT on this same
	// route; there is deliberately no /dsh-startup/config endpoint.)
	// No omitempty: on a fresh install this must serialize as an explicit
	// false, or the client cannot tell "not configured yet" from "field
	// absent". It must also be filled on every read path, or the value ends up
	// meaning "selection.json exists" instead of "setup finished".
	Configured bool `json:"configured"`
}

func themeSelectionPath() string {
	return filepath.Join(themesDir(), "selection.json")
}

// validPackID constrains the pack id the way the web UI writes it: the same
// charset a theme directory name may use, since it is echoed into a
// data-theme-pack attribute and used under /api/themes/{id}/.
func validPackID(id string) bool {
	if len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleGetThemeSelection(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(themeSelectionPath())
	if err != nil {
		if os.IsNotExist(err) {
			// Configured is response-only and must reflect onboarding, not the
			// presence of this file: a user who deletes selection.json has still
			// finished setup, and the corrupt-file branch below already does this.
			writeJSON(w, http.StatusOK, themeSelection{Configured: onboardingDone()})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var sel themeSelection
	if err := json.Unmarshal(data, &sel); err != nil {
		// A corrupted selection must not brick the picker: report none set.
		writeJSON(w, http.StatusOK, themeSelection{Configured: onboardingDone()})
		return
	}
	sel.Configured = onboardingDone()
	writeJSON(w, http.StatusOK, sel)
}

// onboardingDone reports whether first-run setup has been completed or
// explicitly skipped. It feeds the response-only Configured field, which tells
// the client how to play the boot splash (shorter and non-interactive on a
// fresh install) rather than whether to play it.
func onboardingDone() bool {
	return detectOnboardPhase("") == ""
}

func (s *Server) handlePutThemeSelection(w http.ResponseWriter, r *http.Request) {
	var sel themeSelection
	if err := readBodyJSON(r, &sel); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}
	switch sel.Mode {
	case "", "light", "dark", "system":
	default:
		writeError(w, http.StatusBadRequest, "mode must be light, dark or system")
		return
	}
	if !validPackID(sel.Pack) {
		writeError(w, http.StatusBadRequest, "invalid pack id")
		return
	}
	if err := validBackgroundID(sel.Background); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch sel.BackgroundFit {
	case "", "cover", "contain", "fill":
	default:
		writeError(w, http.StatusBadRequest, "backgroundFit must be cover, contain or fill")
		return
	}
	if sel.BackgroundOpacity != nil && (*sel.BackgroundOpacity < 0.05 || *sel.BackgroundOpacity > 0.8) {
		writeError(w, http.StatusBadRequest, "backgroundOpacity must be between 0.05 and 0.8")
		return
	}
	if sel.BackgroundBlur != nil && (*sel.BackgroundBlur < 0 || *sel.BackgroundBlur > 30) {
		writeError(w, http.StatusBadRequest, "backgroundBlur must be between 0 and 30")
		return
	}
	data, err := json.MarshalIndent(sel, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(themesDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(themeSelectionPath(), append(data, '\n'), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sel)
}
