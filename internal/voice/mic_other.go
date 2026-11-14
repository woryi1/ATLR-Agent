//go:build !windows

package voice

import "errors"

// Mic is a no-op stand-in outside Windows: the desktop build is Windows-only,
// but `go build ./...` must still succeed elsewhere.
type Mic struct{}

// StartMic reports that capture is unavailable on this platform.
func StartMic() (*Mic, error) {
	return nil, errors.New("voice: microphone capture is only implemented on Windows")
}

// Read is never reached on an unsupported platform.
func (m *Mic) Read() ([]byte, error) {
	return nil, errors.New("voice: microphone capture is only implemented on Windows")
}

// Stop is a no-op.
func (m *Mic) Stop() {}
