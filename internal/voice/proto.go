// Package voice records from the default microphone and streams the audio to
// Doubao's VoiceGenie ASR, returning the recognised text.
//
// Everything here is server-side on purpose: the webview only flips a button.
// Capturing in the page would need getUserMedia (a permission prompt and a
// secure origin) and would still hand raw audio to the browser; the desktop
// host already owns the machine, so the Go process opens the device directly.
package voice

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Wire format of the VoiceGenie gateway: a hand-rolled protobuf subset.
//
//	request:  2=appkey 3=namespace("VoiceGenie") 5=event 6=payload(json)
//	          7=audio 8=task_id 9=sequence(varint)
//	response: 1=task_id 2=message_id 3=namespace 4=event 5=status_code
//	          6=status_text 7=payload 8=data 9=seq_id 10=session_id 11=log_id
func putVarint(buf []byte, value uint64) []byte {
	for value > 127 {
		buf = append(buf, byte(value&127)|128)
		value >>= 7
	}
	return append(buf, byte(value))
}

func appendField(dst []byte, number int, value []byte) []byte {
	dst = putVarint(dst, uint64(number)<<3|2)
	dst = putVarint(dst, uint64(len(value)))
	return append(dst, value...)
}

// marshalPayload keeps non-ASCII text unescaped, matching the reference client.
func marshalPayload(v any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// EncodeRequest builds one upstream frame. A non-zero sequence rides field 9.
func EncodeRequest(event, appkey, taskID string, payload any, audio []byte, sequence uint64) ([]byte, error) {
	pj, err := marshalPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var packet []byte
	packet = appendField(packet, 2, []byte(appkey))
	packet = appendField(packet, 3, []byte("VoiceGenie"))
	packet = appendField(packet, 5, []byte(event))
	packet = appendField(packet, 6, pj)
	if taskID != "" {
		packet = appendField(packet, 8, []byte(taskID))
	}
	if audio != nil {
		packet = appendField(packet, 7, audio)
	}
	if sequence > 0 {
		packet = putVarint(packet, uint64(9)<<3)
		packet = putVarint(packet, sequence)
	}
	return packet, nil
}

// Response is a decoded downstream frame.
type Response struct {
	TaskID     string
	MessageID  string
	Namespace  string
	Event      string
	StatusCode uint64
	HasCode    bool
	StatusText string
	Payload    string
	Data       []byte
	SeqID      uint64
	SessionID  string
	LogID      string
}

var errTruncated = errors.New("response field truncated")

// DecodeResponse decodes one downstream frame.
func DecodeResponse(data []byte) (*Response, error) {
	names := map[int]string{
		1: "task_id", 2: "message_id", 3: "namespace", 4: "event",
		5: "status_code", 6: "status_text", 7: "payload", 8: "data",
		9: "seq_id", 10: "session_id", 11: "log_id",
	}
	intFields := map[int]bool{5: true, 9: true}

	r := &Response{}
	off := 0
	readVarint := func() (uint64, error) {
		var value uint64
		for i := 0; i < 10; i++ {
			if off >= len(data) {
				return 0, errors.New("varint truncated")
			}
			b := data[off]
			off++
			value |= uint64(b&127) << (i * 7)
			if b&128 == 0 {
				return value, nil
			}
		}
		return 0, errors.New("invalid varint")
	}
	for off < len(data) {
		tag, err := readVarint()
		if err != nil {
			return nil, err
		}
		number, wire := int(tag>>3), int(tag&7)
		if number == 0 {
			return nil, errors.New("invalid field number")
		}
		var value []byte
		switch wire {
		case 0:
			v, err := readVarint()
			if err != nil {
				return nil, err
			}
			// Integers are carried as 8 big-endian bytes so the switch below
			// can treat them uniformly.
			value = binary.BigEndian.AppendUint64(nil, v)
		case 2:
			length, err := readVarint()
			if err != nil {
				return nil, err
			}
			end := off + int(length)
			if end > len(data) || int(length) < 0 {
				return nil, errTruncated
			}
			value = data[off:end]
			off = end
		case 1:
			if off+8 > len(data) {
				return nil, errTruncated
			}
			value = data[off : off+8]
			off += 8
		case 5:
			if off+4 > len(data) {
				return nil, errTruncated
			}
			value = data[off : off+4]
			off += 4
		default:
			return nil, fmt.Errorf("unsupported wire type: %d", wire)
		}
		name, known := names[number]
		if !known {
			continue
		}
		expectInt := intFields[number]
		if expectInt != (wire == 0) {
			return nil, fmt.Errorf("field %s has the wrong wire type", name)
		}
		switch name {
		case "task_id":
			r.TaskID = string(value)
		case "message_id":
			r.MessageID = string(value)
		case "namespace":
			r.Namespace = string(value)
		case "event":
			r.Event = string(value)
		case "status_code":
			if len(value) == 8 {
				r.StatusCode = binary.BigEndian.Uint64(value)
				r.HasCode = true
			}
		case "status_text":
			r.StatusText = string(value)
		case "payload":
			r.Payload = string(value)
		case "data":
			r.Data = value
		case "seq_id":
			if len(value) == 8 {
				r.SeqID = binary.BigEndian.Uint64(value)
			}
		case "session_id":
			r.SessionID = string(value)
		case "log_id":
			r.LogID = string(value)
		}
	}
	return r, nil
}
