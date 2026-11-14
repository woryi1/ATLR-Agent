package voice

import (
	"errors"
	"sync"
	"time"
)

// ErrNotRecording is returned by Stop when nothing is being recorded.
var ErrNotRecording = errors.New("voice: not recording")

const (
	// How long Stop waits for a connect that is still in flight before giving
	// up and closing anyway.
	connectWait = 6 * time.Second
)

type phase int

const (
	phaseIdle phase = iota
	phaseStarting
	phaseRecording
	phaseStopping
)

// Service owns the one microphone session the process may have at a time.
type Service struct {
	mu        sync.Mutex
	phase     phase
	mic       *Mic
	sess      *Session
	pending   [][]byte // audio captured while the gateway was still connecting
	interim   string
	lastErr   error
	connected chan struct{}
}

// NewService returns an idle service.
func NewService() *Service { return &Service{} }

// Start opens the microphone and begins connecting. It returns as soon as the
// device is open — the gateway handshake takes 1–2 s and must not hold the
// button hostage. Audio captured meanwhile is buffered and flushed once the
// session is up, so nothing the user says is lost.
func (s *Service) Start() error {
	s.mu.Lock()
	if s.phase != phaseIdle {
		s.mu.Unlock()
		return nil
	}
	s.phase = phaseStarting
	s.lastErr = nil
	s.interim = ""
	s.mu.Unlock()

	mic, err := StartMic()
	if err != nil {
		s.mu.Lock()
		s.phase = phaseIdle
		s.lastErr = err
		s.mu.Unlock()
		return err
	}

	done := make(chan struct{})
	s.mu.Lock()
	s.mic = mic
	s.connected = done
	s.mu.Unlock()

	go s.pump(mic)

	go func() {
		defer close(done)
		sess, err := NewRealtimeSession(SessionCallbacks{
			OnInterim: func(text string) {
				s.mu.Lock()
				s.interim = text
				s.mu.Unlock()
			},
		})
		s.mu.Lock()
		if err != nil {
			// A failure while stopping is just a cancelled session.
			idle := s.phase == phaseStarting
			if idle {
				s.lastErr = err
				s.phase = phaseIdle
			}
			s.mu.Unlock()
			if idle {
				mic.Stop()
			}
			return
		}
		stopping := s.phase != phaseStarting
		if !stopping {
			s.sess = sess
			s.phase = phaseRecording
		}
		pending := s.pending
		s.pending = nil
		s.mu.Unlock()

		for _, pcm := range pending {
			sess.Feed(pcm)
		}
		if stopping {
			// Stop already ran while we were connecting: finalise right away.
			sess.Finish()
			mic.Stop()
		}
	}()

	return nil
}

func (s *Service) pump(mic *Mic) {
	for {
		pcm, err := mic.Read()
		if err != nil {
			return
		}
		s.mu.Lock()
		sess := s.sess
		if sess == nil {
			// Gateway not ready yet: keep the audio on the side.
			if len(s.pending) < 600 { // ~60 s cap
				s.pending = append(s.pending, pcm)
			}
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		sess.Feed(pcm)
	}
}

// Stop stops capturing, finalises the ASR session and returns the transcript.
// The wait is bounded by the session's own finish window (about 2.5 s), not by
// the 15 s receive timeout: the gateway frequently never sends SessionFinished
// after EndASR, and blocking on it was what made every stop feel stuck.
func (s *Service) Stop() (string, error) {
	s.mu.Lock()
	if s.phase == phaseIdle {
		s.mu.Unlock()
		return "", ErrNotRecording
	}
	mic := s.mic
	connected := s.connected
	s.phase = phaseStopping
	s.mu.Unlock()

	if mic != nil {
		mic.Stop()
	}
	if connected != nil {
		select {
		case <-connected:
		case <-time.After(connectWait):
		}
	}

	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()

	text := ""
	if sess != nil {
		sess.Finish()
		text = sess.FinalText()
	}

	s.mu.Lock()
	s.sess = nil
	s.mic = nil
	s.connected = nil
	s.pending = nil
	s.interim = ""
	s.phase = phaseIdle
	s.mu.Unlock()
	return text, nil
}

// Status reports whether a recording is running, the text recognised so far,
// and the last startup failure (empty when healthy).
func (s *Service) Status() (bool, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	errText := ""
	if s.lastErr != nil {
		errText = s.lastErr.Error()
	}
	return s.phase == phaseStarting || s.phase == phaseRecording || s.phase == phaseStopping, s.interim, errText
}
