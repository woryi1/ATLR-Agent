package server

import (
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/open-octo/octo-agent/internal/datahome"
	"github.com/open-octo/octo-agent/internal/version"
)

// ─── GET /api/version ────────────────────────────────────────────────────────

// handleVersion reports the running build's identity: version, platform, and
// whether the caller may use native/browser-hosted affordances. Mobile clients
// and the web UI use it both as a liveness probe (it is unauthenticated and
// cheap) and as the source of the displayed version number.
//
// It deliberately performs no network I/O. This build carries no update
// channel: there is no latest-release lookup, no update verdict and no
// download URL, so a read is a pure local answer.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	// The response includes the running binary version, which changes after a
	// restart. Tell browsers and intermediaries not to cache it so a re-created
	// window reflects the running server immediately.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	response := map[string]any{
		"version": version.Version,
		// current is the version with any leading "v" stripped — the form the
		// UI renders. Kept because older clients read it.
		"current": strings.TrimPrefix(version.Version, "v"),
		// cli_command is a constant — the endpoint is unauthenticated, so the
		// executable's filesystem path must not leak here.
		"cli_command": "octo",
		// native is true in the Wails desktop build (a NativeBridge is wired):
		// the frontend can use OS dialogs/notifications. local is true when the
		// browser is genuinely on this machine — desktop or localhost web —
		// telling the frontend to pick files/folders by real path instead of
		// uploading. A peer that merely reaches us over loopback because
		// something forwards the port (ngrok, a proxy, cloudflared) is not
		// local and falls back to upload; see isLocalRequest.
		"native": s.cfg.Native != nil,
		"local":  isLocalRequest(r),
		// os lets the frontend gate platform-specific UI (e.g. the experimental
		// computer-use toggle is meaningful only on the macOS and Windows desktop).
		"os": runtime.GOOS,
		// os_version is the host's macOS product version (e.g. "26.5.2"; empty
		// elsewhere). The titlebar rows read it to sit on the traffic lights'
		// axis, which macOS 26 moved for windows stamped with the macOS 26 SDK.
		"os_version": OSVersion(),
	}
	if profile := strings.TrimSpace(os.Getenv(datahome.ProfileEnv)); profile != "" {
		response["profile"] = profile
	}
	writeJSON(w, http.StatusOK, response)
}
