package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-octo/octo-agent/internal/datahome"
)

// persistEnv points the portable data root at a temp dir so Save() and
// LoadSession() resolve the SAME sessions directory without touching the real
// Data tree. Root() reads the env on every call, so there is no cached path
// leaking between these (non-parallel) tests.
func persistEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv(string(datahome.PortableRootEnv), tmp)
	t.Setenv(datahome.ProfileEnv, "")
	return tmp
}

// roundHistory is the shape of one turn that carried two silent notes: the
// user turn, an assistant tool_use round, the tool_result batch, then the two
// notes at the tail of the tool log, then the final answer. Plain text is used
// for the tool_result stand-in so the assertion isolates role/content/order —
// the exact dimensions the prompt-cache prefix depends on.
func roundHistory() (*History, []Message) {
	h := NewHistory()
	msgs := []Message{
		NewUserMessage("run the tool"),
		NewAssistantMessage("calling bash"),
		NewUserMessage("tool results here"), // stand-in for the tool_result batch
		SilentNote("also check X"),
		SilentNote("and print Y"),
		NewAssistantMessage("done"),
	}
	for _, m := range msgs {
		h.Append(m)
	}
	return h, msgs
}

func silentTexts(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		if IsSilentNote(m) {
			out = append(out, strings.TrimPrefix(m.Content, SilentNotePrefix))
		}
	}
	return out
}

// messageLines reads the transcript and returns the raw bytes of every
// "message" record line, in file order.
func messageLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var out []string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec sessionRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("parse record: %v", err)
		}
		if rec.Type == "message" {
			out = append(out, string(line))
		}
	}
	return out
}

// TestSilentNote_AppendedThenPersisted (verification ①): a silent note written
// through the exact drain constructor reaches the session file as a normal
// "message" record carrying the silent prefix — it is not kept only in memory.
func TestSilentNote_AppendedThenPersisted(t *testing.T) {
	persistEnv(t)
	h, _ := roundHistory()
	sess := NewSession("m", "")
	sess.SyncFrom(h)
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, err := sess.SavePath()
	if err != nil {
		t.Fatalf("SavePath: %v", err)
	}
	lines := messageLines(t, path)
	if len(lines) != h.Len() {
		t.Fatalf("persisted %d message lines, want %d", len(lines), h.Len())
	}
	marked := 0
	for _, l := range lines {
		if strings.Contains(l, SilentNotePrefix) {
			marked++
		}
	}
	if marked != 2 {
		t.Fatalf("session file has %d lines with the silent prefix, want 2:\n%s",
			marked, strings.Join(lines, "\n"))
	}
}

// TestSilentNote_ReloadedHistoryIsIdentical (verification ②): the runtime
// history and the "persist -> reload" history are equal element-by-element in
// role + content + order, AND every persisted line is byte-identical to the
// runtime Message serialized through the same messageRecord the live Save uses.
// That is the prefix-invariance guarantee the prompt cache relies on.
func TestSilentNote_ReloadedHistoryIsIdentical(t *testing.T) {
	persistEnv(t)
	h, _ := roundHistory()
	snap := h.Snapshot()

	sess := NewSession("m", "")
	sess.SyncFrom(h)
	if err := sess.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// (a) byte-level: file lines == json.Marshal(messageRecord(m)) of the
	// runtime history. No timestamps/ids were added or reordered.
	path, _ := sess.SavePath()
	lines := messageLines(t, path)
	for i, m := range snap {
		want, err := json.Marshal(messageRecord(m))
		if err != nil {
			t.Fatalf("marshal runtime message %d: %v", i, err)
		}
		if lines[i] != string(want) {
			t.Errorf("line %d differs from runtime serialization:\n got  %s\n want %s",
				i, lines[i], want)
		}
	}

	// (b) reload through the real LoadSession/ToHistory path used on resume.
	loaded, err := LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	reloaded := loaded.ToHistory().Snapshot()
	if len(reloaded) != len(snap) {
		t.Fatalf("reloaded len = %d, want %d", len(reloaded), len(snap))
	}
	for i := range snap {
		if reloaded[i].Role != snap[i].Role || reloaded[i].Content != snap[i].Content {
			t.Errorf("msg[%d]: reloaded role/content = %q/%q, runtime = %q/%q",
				i, reloaded[i].Role, reloaded[i].Content, snap[i].Role, snap[i].Content)
		}
		if !reflect.DeepEqual(reloaded[i].Blocks, snap[i].Blocks) {
			t.Errorf("msg[%d]: blocks differ after reload", i)
		}
	}
	// Position of the notes is unchanged (tool-log tail), so the prefix is.
	wantNotes := []string{"also check X", "and print Y"}
	if got := silentTexts(reloaded); !reflect.DeepEqual(got, wantNotes) {
		t.Errorf("reloaded silent notes = %v, want %v (order/position drifted)", got, wantNotes)
	}
	if got := silentTexts(snap); !reflect.DeepEqual(got, wantNotes) {
		t.Errorf("runtime silent notes = %v, want %v", got, wantNotes)
	}
}

// TestSilentNote_DrainIsIdempotent (verification ③): Drain() empties the queue,
// so the same note can never be appended twice by a later drain/teardown.
func TestSilentNote_DrainIsIdempotent(t *testing.T) {
	var ib Inbox
	ib.EnqueueSilent("a")
	ib.EnqueueSilent("b")

	first := ib.Drain()
	if len(first) != 2 {
		t.Fatalf("first Drain returned %d items, want 2", len(first))
	}
	if second := ib.Drain(); len(second) != 0 {
		t.Fatalf("second Drain returned %d items, want 0 (queue must be cleared)", len(second))
	}

	// Simulate the loop drain + a later teardown both consuming the same queue:
	// only the first Drain's items are appended, the second adds nothing.
	h := NewHistory()
	for _, it := range first {
		h.Append(SilentNote(it.Text))
	}
	for _, it := range ib.Drain() { // empty — the "second" consumer
		h.Append(SilentNote(it.Text))
	}
	if got := silentTexts(h.Snapshot()); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("notes appended twice or lost: %v", got)
	}
}

// TestSilentNote_LegacySessionWithoutSilentField (verification ④): an old
// transcript with no silent notes and no per-message created_at loads with its
// messages intact, no message is misclassified as silent, and re-saving it via
// the append path does not corrupt it.
func TestSilentNote_LegacySessionWithoutSilentField(t *testing.T) {
	persistEnv(t)
	dir, err := SessionsDir()
	if err != nil {
		t.Fatalf("SessionsDir: %v", err)
	}
	legacy := strings.Join([]string{
		`{"type":"meta","id":"legacy-1","model":"m"}`,
		`{"type":"message","message":{"role":"user","content":"old question"}}`,
		`{"type":"message","message":{"role":"assistant","content":"old answer"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "legacy-1.jsonl"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy transcript: %v", err)
	}

	s, err := LoadSession("legacy-1")
	if err != nil {
		t.Fatalf("LoadSession(legacy): %v", err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("legacy messages = %d, want 2", len(s.Messages))
	}
	if s.Messages[0].Content != "old question" || s.Messages[0].Role != RoleUser ||
		s.Messages[1].Content != "old answer" || s.Messages[1].Role != RoleAssistant {
		t.Errorf("legacy messages changed: %+v", s.Messages)
	}
	for _, m := range s.Messages {
		if IsSilentNote(m) {
			t.Errorf("legacy message misclassified as silent: %q", m.Content)
		}
	}
	// A resumed legacy session still appends cleanly (no silent field anywhere).
	h := s.ToHistory()
	h.Append(NewUserMessage("new question"))
	s.SyncFrom(h)
	if err := s.Save(); err != nil {
		t.Fatalf("Save(legacy): %v", err)
	}
	got := messageLines(t, filepath.Join(dir, "legacy-1.jsonl"))
	if len(got) != 3 {
		t.Fatalf("after append legacy has %d message lines, want 3", len(got))
	}
	if strings.Contains(strings.Join(got, "\n"), SilentNotePrefix) {
		t.Error("legacy transcript unexpectedly gained a silent prefix")
	}
}
