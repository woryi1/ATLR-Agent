package server

// Voice input: the webview only toggles a button; capture and recognition run
// in the Go process, so there is no getUserMedia permission prompt and no audio
// crossing the page boundary.
//
// The service is process-wide on purpose — a machine has one default
// microphone, and two concurrent sessions would fight over it.

import (
	"net/http"

	"github.com/open-octo/octo-agent/internal/voice"
)

var voiceService = voice.NewService()

func (s *Server) handleVoiceStart(w http.ResponseWriter, r *http.Request) {
	if err := voiceService.Start(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recording": true})
}

func (s *Server) handleVoiceStop(w http.ResponseWriter, r *http.Request) {
	text, err := voiceService.Stop()
	if err != nil {
		status := http.StatusInternalServerError
		if err == voice.ErrNotRecording {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text})
}

func (s *Server) handleVoiceStatus(w http.ResponseWriter, r *http.Request) {
	recording, interim, errText := voiceService.Status()
	writeJSON(w, http.StatusOK, map[string]any{"recording": recording, "interim": interim, "error": errText})
}
