package server

// Restart support for the desktop shell.
//
// The server runs in-process inside the Wails app, so "restart" means relaunch
// the executable — only the shell knows how to do that. The CLI leaves the hook
// unset and the endpoint reports that it is unavailable.

import (
	"net/http"
	"sync"
	"time"
)

var (
	restartMu   sync.Mutex
	restartHook func()
)

// SetRestartHook registers what the host does when the UI asks for a restart.
func SetRestartHook(fn func()) {
	restartMu.Lock()
	restartHook = fn
	restartMu.Unlock()
}

func (s *Server) handleAppRestart(w http.ResponseWriter, r *http.Request) {
	restartMu.Lock()
	hook := restartHook
	restartMu.Unlock()
	if hook == nil {
		writeError(w, http.StatusNotImplemented, "restart is not available in this build")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	// Let the response reach the page before the process goes away.
	go func() {
		time.Sleep(250 * time.Millisecond)
		hook()
	}()
}
