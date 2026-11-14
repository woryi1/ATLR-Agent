package voice

// Streaming ASR session against the VoiceGenie gateway:
// StartTask → TaskStarted → StartSession → SessionStarted →
// TaskRequest(seq, pcm)*N → EndASR → ASREnded → FinishSession → SessionFinished.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	gateway     = "wss://frontier-audio-web-ws.doubao.com/api/v2/sami/voicegenie"
	appkey      = "GOqQpfo1fO7slHv8" // public web client id, not an account credential
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36"
	recvTimeout = 15 * time.Second
	// finishWait bounds how long Stop blocks after EndASR; see Finish.
	finishWait = 2500 * time.Millisecond
)

// SessionCallbacks are the knots a session reports through while it runs.
type SessionCallbacks struct {
	OnInterim func(text string) // unconfirmed text (confirmed + current interim)
	OnError   func(err error)
	OnDone    func()
}

// Session is one hold-to-talk recognition session.
type Session struct {
	mu      sync.Mutex
	ws      *websocket.Conn
	taskID  string
	seq     uint64
	finals  []string
	interim string
	ready   chan struct{}
	taskOk  chan struct{}
	taskErr chan error
	done    chan struct{}
	asrEnd  chan struct{}
	closed  bool
	cb      SessionCallbacks
}

// NewRealtimeSession dials the gateway and completes the handshake.
func NewRealtimeSession(cb SessionCallbacks) (*Session, error) {
	boot, err := cachedBootstrap()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("api_app_key", appkey)
	q.Set("namespace", "VoiceGenie")
	q.Set("version_code", "20800")
	q.Set("language", "zh")
	q.Set("device_platform", "web")
	q.Set("pkg_type", "release_version")
	q.Set("pc_version", boot.PCVersion)
	q.Set("doubao_pc_version", boot.PCVersion)
	q.Set("region", "CN")
	q.Set("sys_region", "CN")
	q.Set("samantha_web", "1")
	q.Set("use-olympus-account", "1")
	q.Set("doubao_device_platform", "web")
	q.Set("aid", boot.AID)
	q.Set("real_aid", boot.AID)
	q.Set("device_id", boot.WebID)
	q.Set("web_platform", "browser")
	header := http.Header{}
	header.Set("User-Agent", userAgent)
	header.Set("Origin", "https://www.doubao.com")
	for _, c := range boot.Cookies {
		header.Add("Cookie", c.Name+"="+c.Value)
	}
	ws, _, err := websocket.DefaultDialer.Dial(gateway+"?"+q.Encode(), header)
	if err != nil {
		return nil, fmt.Errorf("dial voice gateway: %w", err)
	}
	s := &Session{
		ws:      ws,
		ready:   make(chan struct{}),
		taskOk:  make(chan struct{}),
		taskErr: make(chan error, 1),
		done:    make(chan struct{}),
		asrEnd:  make(chan struct{}),
		cb:      cb,
	}
	go s.readLoop()
	if err := s.start(); err != nil {
		ws.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) send(event string, payload any, audio []byte) error {
	return s.sendSeq(event, payload, audio, 0)
}

func (s *Session) sendSeq(event string, payload any, audio []byte, seq uint64) error {
	frame, err := EncodeRequest(event, appkey, s.taskID, payload, audio, seq)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("voice: session closed")
	}
	_ = s.ws.SetWriteDeadline(time.Now().Add(recvTimeout))
	return s.ws.WriteMessage(websocket.BinaryMessage, frame)
}

func (s *Session) start() error {
	if err := s.send("StartTask", nil, nil); err != nil {
		return err
	}
	select {
	case <-s.taskOk:
	case err := <-s.taskErr:
		return err
	case <-time.After(recvTimeout):
		return fmt.Errorf("voice: timed out waiting for TaskStarted")
	}
	if err := s.send("StartSession", map[string]any{
		"business":     1,
		"request_type": 3,
		"asr": map[string]any{
			"audio_info": map[string]any{"channel": 1, "format": "pcm", "sample_rate": 16000},
		},
	}, nil); err != nil {
		return err
	}
	select {
	case <-s.ready:
	case err := <-s.taskErr:
		return err
	case <-time.After(recvTimeout):
		return fmt.Errorf("voice: timed out waiting for SessionStarted")
	}
	return nil
}

// Feed pushes one 16 kHz / 16-bit / mono PCM chunk.
func (s *Session) Feed(pcm []byte) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	_ = s.sendSeq("TaskRequest", nil, pcm, seq)
}

// Finish ends the ASR stream and waits briefly for the last result. The wait is
// deliberately short: the reference client blocks on SessionFinished, which the
// gateway routinely never sends after EndASR — every stop then sat through the
// full 15 s receive timeout waiting for an event that was not coming. ASREnded
// is the real "final transcript is in" signal; we take it (with a short cap in
// case it never arrives) and close the socket ourselves.
func (s *Session) Finish() {
	_ = s.send("EndASR", map[string]any{}, nil)
	select {
	case <-s.asrEnd:
		// Give the last ASRResponse frame a moment to land before tearing down.
		time.Sleep(120 * time.Millisecond)
	case <-s.done:
	case <-time.After(finishWait):
	}
	s.abort("FinishSession")
}

func (s *Session) abort(event string) {
	s.mu.Lock()
	closed := s.closed
	s.closed = true
	ws := s.ws
	s.mu.Unlock()
	if !closed {
		_ = s.send(event, map[string]any{}, nil)
	}
	ws.Close()
}

func (s *Session) readLoop() {
	defer close(s.done)
	for {
		_ = s.ws.SetReadDeadline(time.Now().Add(recvTimeout))
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				log.Printf("[voice] read loop exited: %v", err)
			}
			return
		}
		resp, err := DecodeResponse(data)
		if err != nil {
			log.Printf("[voice] bad frame: %v", err)
			continue
		}
		s.dispatch(resp)
	}
}

func (s *Session) dispatch(resp *Response) {
	if (resp.HasCode && resp.StatusCode != 20000000) || resp.Event == "TaskFailed" || resp.Event == "SessionFailed" {
		select {
		case s.taskErr <- fmt.Errorf("voice service failed: %s / %s", resp.Event, resp.StatusText):
		default:
		}
		s.abort("FinishSession")
		return
	}
	switch resp.Event {
	case "TaskStarted":
		if resp.TaskID != "" {
			s.taskID = resp.TaskID
		}
		close(s.taskOk)
	case "SessionStarted":
		close(s.ready)
	case "ASRResponse":
		s.handleASR(resp.Payload)
	case "FailedTask":
		text := resp.StatusText
		if text == "" {
			text = "voice gateway returned a failure"
		}
		select {
		case s.taskErr <- fmt.Errorf("%s", text):
		default:
		}
		s.abort("FinishSession")
	case "ASREnded":
		select {
		case <-s.asrEnd:
		default:
			close(s.asrEnd)
		}
	case "SessionFinished":
		s.mu.Lock()
		s.closed = true
		ws := s.ws
		s.mu.Unlock()
		ws.Close()
		if s.cb.OnDone != nil {
			s.cb.OnDone()
		}
	}
}

type asrPayload struct {
	Results []struct {
		Text      string `json:"text"`
		IsInterim bool   `json:"is_interim"`
	} `json:"results"`
}

func (s *Session) handleASR(payloadJSON string) {
	if payloadJSON == "" {
		return
	}
	var payload asrPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		log.Printf("[voice] bad ASR payload: %v", err)
		return
	}
	for _, r := range payload.Results {
		if r.IsInterim {
			s.interim = r.Text
		} else {
			if r.Text != "" {
				s.finals = append(s.finals, r.Text)
			}
			s.interim = ""
		}
	}
	if s.cb.OnInterim != nil {
		if text := s.InterimText(); text != "" {
			s.cb.OnInterim(text)
		}
	}
}

func (s *Session) confirmedText() string { return strings.Join(s.finals, "") }

// InterimText is what the UI should show while recording (confirmed + interim).
func (s *Session) InterimText() string { return s.confirmedText() + s.interim }

// FinalText is the complete confirmed transcript.
func (s *Session) FinalText() string { return s.confirmedText() }
