//go:build windows

package voice

// winmm waveIn capture (16 kHz / 16-bit / mono). Pure Go: NewLazySystemDLL plus
// CALLBACK_EVENT with eight rotating buffers, so no cgo and no third-party
// audio dependency.

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	sampleRate    = 16000
	bitsPerSample = 16
	channels      = 1
	bufCount      = 8
	bufBytes      = 3200 // 100 ms
)

var (
	winmm                     = windows.NewLazySystemDLL("winmm.dll")
	procWaveInOpen            = winmm.NewProc("waveInOpen")
	procWaveInStart           = winmm.NewProc("waveInStart")
	procWaveInStop            = winmm.NewProc("waveInStop")
	procWaveInReset           = winmm.NewProc("waveInReset")
	procWaveInClose           = winmm.NewProc("waveInClose")
	procWaveInPrepareHeader   = winmm.NewProc("waveInPrepareHeader")
	procWaveInUnprepareHeader = winmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuffer       = winmm.NewProc("waveInAddBuffer")
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

const (
	waveMapper    = ^uint32(0) // WAVE_MAPPER
	callbackEvent = 0x50000    // CALLBACK_EVENT
	hdrDone       = 1          // WHDR_DONE
	mmsysNoError  = 0
)

// Mic is one recording session on the default input device.
type Mic struct {
	device   uintptr
	event    windows.Handle
	headers  []waveHdr
	buffers  [][]byte
	stopped  bool
	nextRead int
}

// StartMic opens the default microphone and starts capturing.
func StartMic() (*Mic, error) {
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	format := waveFormatEx{
		FormatTag:      1, // PCM
		Channels:       channels,
		SamplesPerSec:  sampleRate,
		AvgBytesPerSec: sampleRate * channels * bitsPerSample / 8,
		BlockAlign:     channels * bitsPerSample / 8,
		BitsPerSample:  bitsPerSample,
	}
	m := &Mic{event: event}
	var device uintptr
	ret, _, _ := procWaveInOpen.Call(
		uintptr(unsafe.Pointer(&device)),
		uintptr(waveMapper),
		uintptr(unsafe.Pointer(&format)),
		uintptr(event), 0, callbackEvent)
	if ret != mmsysNoError {
		windows.CloseHandle(event)
		return nil, fmt.Errorf("waveInOpen failed (mmsys error %d) — check microphone access", ret)
	}
	m.device = device
	for i := 0; i < bufCount; i++ {
		buf := make([]byte, bufBytes)
		hdr := waveHdr{Data: uintptr(unsafe.Pointer(&buf[0])), BufferLength: bufBytes}
		ret, _, _ := procWaveInPrepareHeader.Call(device, uintptr(unsafe.Pointer(&hdr)), unsafe.Sizeof(hdr))
		if ret != mmsysNoError {
			m.cleanup()
			return nil, errors.New("waveInPrepareHeader failed")
		}
		m.buffers = append(m.buffers, buf)
		m.headers = append(m.headers, hdr)
		procWaveInAddBuffer.Call(device, uintptr(unsafe.Pointer(&m.headers[i])), unsafe.Sizeof(hdr))
	}
	if ret, _, _ := procWaveInStart.Call(device); ret != mmsysNoError {
		m.cleanup()
		return nil, errors.New("waveInStart failed")
	}
	// Keep the header array on the Go heap for as long as winmm holds pointers
	// into it.
	runtime.KeepAlive(m.headers)
	return m, nil
}

// Read blocks until the next 100 ms PCM block is ready.
func (m *Mic) Read() ([]byte, error) {
	for {
		if m.stopped {
			return nil, errors.New("voice: recording stopped")
		}
		windows.WaitForSingleObject(m.event, windows.INFINITE)
		for i := 0; i < bufCount; i++ {
			idx := (m.nextRead + i) % bufCount
			hdr := &m.headers[idx]
			if hdr.Flags&hdrDone != 0 {
				m.nextRead = (idx + 1) % bufCount
				data := make([]byte, hdr.BytesRecorded)
				copy(data, m.buffers[idx][:hdr.BytesRecorded])
				hdr.Flags &^= hdrDone
				hdr.BytesRecorded = 0
				procWaveInAddBuffer.Call(m.device, uintptr(unsafe.Pointer(hdr)), unsafe.Sizeof(*hdr))
				return data, nil
			}
		}
	}
}

// Stop stops capturing and releases the device.
func (m *Mic) Stop() {
	if m.stopped {
		return
	}
	m.stopped = true
	windows.SetEvent(m.event) // unblock a pending Read
	procWaveInStop.Call(m.device)
	procWaveInReset.Call(m.device)
	m.cleanup()
}

func (m *Mic) cleanup() {
	for i := range m.headers {
		procWaveInUnprepareHeader.Call(m.device, uintptr(unsafe.Pointer(&m.headers[i])), unsafe.Sizeof(m.headers[i]))
	}
	procWaveInClose.Call(m.device)
	windows.CloseHandle(m.event)
}
