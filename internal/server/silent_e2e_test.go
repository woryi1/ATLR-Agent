package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-octo/octo-agent/internal/agent"
)

// silentE2ESender drives a two-round tool turn (same shape as
// incremental_persist_test.go's blockingToolSender) and additionally records
// the exact []Message the provider received on round 2 — i.e. the history the
// ORIGINAL turn constructed, including the drained silent note. That snapshot
// is what gets re-sent after a restart; comparing it proves the prefix is
// unchanged (and therefore the prompt cache still hits).
type silentE2ESender struct {
	mu         sync.Mutex
	round      int
	entered1   chan struct{}
	release1   chan struct{}
	entered2   chan struct{}
	release2   chan struct{}
	round2Msgs []agent.Message
}

func (s *silentE2ESender) SendMessages(context.Context, string, string, []agent.Message, int) (agent.Reply, error) {
	return agent.Reply{Content: "side-call"}, nil
}
func (s *silentE2ESender) StreamMessages(context.Context, string, string, []agent.Message, int, func(string), func(string)) (agent.Reply, error) {
	return agent.Reply{Content: "side-call"}, nil
}
func (s *silentE2ESender) SendMessagesWithTools(context.Context, string, string, []agent.Message, int, []agent.ToolDefinition) (agent.Reply, error) {
	return agent.Reply{Content: "side-call"}, nil
}
func (s *silentE2ESender) StreamMessagesWithTools(_ context.Context, _, _ string, msgs []agent.Message, _ int, _ []agent.ToolDefinition, _ func(string), _ agent.ToolInputDeltaFunc, _ agent.ThinkingDeltaFunc) (agent.Reply, error) {
	s.mu.Lock()
	s.round++
	round := s.round
	if round == 2 {
		s.round2Msgs = append([]agent.Message(nil), msgs...)
	}
	s.mu.Unlock()
	switch round {
	case 1:
		close(s.entered1)
		<-s.release1
		return agent.Reply{
			Blocks:     []agent.ContentBlock{agent.NewToolUseBlock("tu1", "read_file", map[string]any{"path": "/nonexistent/silent-e2e"})},
			StopReason: "tool_use",
		}, nil
	case 2:
		close(s.entered2)
		<-s.release2
		return agent.Reply{Content: "done"}, nil
	default:
		return agent.Reply{Content: "side-call"}, nil
	}
}

// msgKey is the role+content projection the prompt-cache prefix depends on.
func msgKey(m agent.Message) string { return string(m.Role) + "\x00" + m.Content }

func keys(msgs []agent.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = msgKey(m)
	}
	return out
}

// TestSilentNote_EndToEnd_PersistReloadResend is the full-stack proof:
//
//  1. a REAL WS turn (doAgentTurn) is in flight;
//  2. POST /api/sessions/{id}/silent-message (the real HTTP handler) queues a
//     note mid-turn and returns 200 {"queued":true};
//  3. the loop drains it into history, the server persists the session file;
//  4. the transcript on disk contains "[用户补充·静默]";
//  5. "restart the service" = drop every in-memory object and LoadSession(id)
//     from disk only, exactly what buildAgent does after a restart;
//  6. the reloaded history still carries the note, at the same position;
//  7. the messages that would be RE-SENT (ToHistory().Snapshot(), the exact
//     slice handed to the provider by agent.go) have the original round's
//     messages as an unchanged byte-for-byte prefix -> prompt cache hits.
func TestSilentNote_EndToEnd_PersistReloadResend(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("OCTO_PORTABLE_ROOT", tmp)
	t.Setenv("OCTO_PROFILE", "")

	sender := &silentE2ESender{
		entered1: make(chan struct{}), release1: make(chan struct{}),
		entered2: make(chan struct{}), release2: make(chan struct{}),
	}
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: true})
	srv.sender = sender
	srv.initWS()
	srv.turnRunning = make(map[string]bool)
	srv.steerQueues = make(map[string][]queuedTurn)
	srv.sessionAgents = make(map[string]*agent.Agent)

	sess := agent.NewSession("stub-model", "")
	sess.Title = "fixed title" // suppress the async title-generation side-call
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.doAgentTurn(sess, "hello", nil, nil)
	}()
	releaseOnce := func(ch chan struct{}) {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	t.Cleanup(func() {
		releaseOnce(sender.release1)
		releaseOnce(sender.release2)
		<-done
	})

	// Round 1 blocked: the agent is registered, so the silent endpoint has a
	// running turn to attach to. Go through the REAL HTTP handler.
	select {
	case <-sender.entered1:
	case <-time.After(5 * time.Second):
		t.Fatal("round 1 never started")
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/sessions/"+sess.ID+"/silent-message",
		strings.NewReader(`{"text":"also check X"}`))
	req.SetPathValue("id", sess.ID)
	rec := httptest.NewRecorder()
	srv.handleSilentMessage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("silent POST = %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	var qresp struct {
		Queued  bool `json:"queued"`
		Pending int  `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &qresp); err != nil || !qresp.Queued {
		t.Fatalf("silent POST body = %s, want queued:true", rec.Body.String())
	}
	releaseOnce(sender.release1)

	// Round 2 blocked inside the provider call: the drained note is now in
	// the messages the model is being sent.
	select {
	case <-sender.entered2:
	case <-time.After(5 * time.Second):
		t.Fatal("round 2 never started")
	}
	sender.mu.Lock()
	runtimeMsgs := append([]agent.Message(nil), sender.round2Msgs...)
	sender.mu.Unlock()
	if len(runtimeMsgs) == 0 {
		t.Fatal("round 2 recorded no messages")
	}
	if !containsSilent(runtimeMsgs) {
		t.Fatalf("runtime round-2 messages lack the silent note: %v", keys(runtimeMsgs))
	}

	// Finish the turn: doAgentTurn does the final SyncFrom+Save.
	releaseOnce(sender.release2)
	<-done

	// (4) The transcript on disk carries the silent note.
	path, err := sess.SavePath()
	if err != nil {
		t.Fatalf("SavePath: %v", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(onDisk), agent.SilentNotePrefix+"also check X") {
		t.Fatalf("session file has no silent note:\n%s", onDisk)
	}
	t.Logf("session file: %s", path)
	for _, line := range strings.Split(string(onDisk), "\n") {
		if strings.Contains(line, agent.SilentNotePrefix) {
			t.Logf("persisted silent line: %s", line)
		}
	}

	// (5) Restart: forget srv/sess entirely, rebuild from disk by id only.
	sid := sess.ID
	sess, srv = nil, nil
	reloaded, err := agent.LoadSession(sid)
	if err != nil {
		t.Fatalf("LoadSession after restart: %v", err)
	}

	// (6) Note still present, in the same tool-log-tail position.
	if !containsSilent(reloaded.Messages) {
		t.Fatalf("reloaded history lost the silent note: %v", keys(reloaded.Messages))
	}
	noteIdx := -1
	for i, m := range reloaded.Messages {
		if agent.IsSilentNote(m) {
			noteIdx = i
		}
	}
	if noteIdx <= 0 || noteIdx >= len(reloaded.Messages)-1 {
		t.Errorf("silent note at index %d of %d — expected tool-log tail (before final assistant)",
			noteIdx, len(reloaded.Messages))
	}
	t.Logf("reloaded history (%d messages), silent note at index %d:", len(reloaded.Messages), noteIdx)
	for i, m := range reloaded.Messages {
		t.Logf("  [%d] role=%-9s content=%q", i, m.Role, m.Content)
	}

	// (7) Prefix invariance: the messages re-sent after restart have the
	// original round's messages as an unchanged prefix (role+content+order).
	resend := reloaded.ToHistory().Snapshot()
	if len(resend) < len(runtimeMsgs) {
		t.Fatalf("re-sent messages (%d) shorter than original round (%d)", len(resend), len(runtimeMsgs))
	}
	if got, want := keys(resend[:len(runtimeMsgs)]), keys(runtimeMsgs); !reflect.DeepEqual(got, want) {
		t.Errorf("prefix drifted!\n original round: %v\n after restart:  %v", want, got)
	}
	// The note is inside that preserved prefix, so the cache-friendly span
	// that contains it is byte-identical too.
	if !containsSilent(resend[:len(runtimeMsgs)]) {
		t.Error("silent note not inside the preserved prefix")
	}
	t.Logf("prefix invariance: original round sent %d msgs; after restart %d msgs would be re-sent; first %d are byte-equal (role+content+order) = %v -> prompt-cache prefix unchanged",
		len(runtimeMsgs), len(resend), len(runtimeMsgs),
		reflect.DeepEqual(keys(resend[:len(runtimeMsgs)]), keys(runtimeMsgs)))
}

func containsSilent(msgs []agent.Message) bool {
	for _, m := range msgs {
		if agent.IsSilentNote(m) {
			return true
		}
	}
	return false
}
