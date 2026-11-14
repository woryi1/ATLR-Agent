package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// silentNotesIn returns the history entries carrying the silent-note marker, in
// order, with the marker stripped.
func silentNotesIn(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role != RoleUser {
			continue
		}
		if strings.HasPrefix(m.Content, SilentNotePrefix) {
			out = append(out, strings.TrimPrefix(m.Content, SilentNotePrefix))
		}
	}
	return out
}

// TestAgent_SilentNotes_AppendedAfterToolBatch is the core contract: two notes
// queued while the turn is running appear, in order, at the tail of the tool
// log — after the tool_result, before the next assistant message — and the
// model is actually sent them on the following round.
func TestAgent_SilentNotes_AppendedAfterToolBatch(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{
			{
				StopReason: "tool_use",
				Blocks: []ContentBlock{
					NewToolUseBlock("call-1", "bash", map[string]any{"command": "echo hi"}),
				},
			},
			{Content: "done", StopReason: "end_turn"},
		},
	}
	exec := &fakeExecutor{results: map[string]string{"bash": "hi"}}
	a := New(send, "m")

	// Queued while round 0 is in flight, i.e. before the tool batch runs.
	send.onCall = func(idx int) {
		if idx == 0 {
			a.Inbox.EnqueueSilent("first aside")
			a.Inbox.EnqueueSilent("second aside")
		}
	}

	var steerEvents int
	handler := func(ev AgentEvent) {
		if ev.Kind == EventSteerInjected {
			steerEvents++
		}
	}

	if _, err := a.RunStream(context.Background(), "run the tool",
		[]ToolDefinition{{Name: "bash", Description: "run shell"}}, exec, handler); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	snap := a.History.Snapshot()
	// user(q), assistant(tool_use), user(tool_result), user(silent), user(silent), assistant(done)
	if len(snap) != 6 {
		t.Fatalf("history len = %d, want 6: %+v", len(snap), snap)
	}
	// Notes land immediately after the tool_result message.
	if snap[2].Role != RoleUser || snap[2].Blocks[0].Type != "tool_result" {
		t.Fatalf("snap[2] is not the tool_result batch: %+v", snap[2])
	}
	got := silentNotesIn(snap)
	if len(got) != 2 || got[0] != "first aside" || got[1] != "second aside" {
		t.Fatalf("silent notes in history = %q, want [first aside, second aside]", got)
	}
	// Order in history matches queue order, and both sit after the tool result.
	if snap[3].Content != SilentNotePrefix+"first aside" || snap[4].Content != SilentNotePrefix+"second aside" {
		t.Errorf("notes landed at the wrong positions: %q / %q", snap[3].Content, snap[4].Content)
	}
	// A silent note is not user speech: no steer event, no transcript echo.
	if steerEvents != 0 {
		t.Errorf("got %d steer events, want 0 (a silent note must not echo as a steer)", steerEvents)
	}
	// The provider actually received them, in order, on the follow-up round.
	seen := silentNotesIn(send.gotMsgs)
	if len(seen) != 2 || seen[0] != "first aside" || seen[1] != "second aside" {
		t.Errorf("provider saw notes %q, want both in order", seen)
	}
	// Tool pairing survived: every tool_use still has a following tool_result.
	if orphans := orphanedToolUses(send.gotMsgs); len(orphans) != 0 {
		t.Errorf("tool_use blocks left without a matching tool_result: %v", orphans)
	}
	if a.Inbox.HasPending() {
		t.Errorf("inbox still has pending items after the turn")
	}
}

// TestAgent_SilentNotes_DoNotStartAnExtraRound: a note that arrives after the
// model's final answer (no more tool calls) must not add a round — the turn is
// over and the note is dropped with it.
func TestAgent_SilentNotes_DoNotStartAnExtraRound(t *testing.T) {
	send := &fakeToolSender{
		replies: []Reply{{Content: "the answer", StopReason: "end_turn"}},
	}
	a := New(send, "m")
	send.onCall = func(idx int) {
		if idx == 0 {
			a.Inbox.EnqueueSilent("too late")
		}
	}
	reply, err := a.Run(context.Background(), "q",
		[]ToolDefinition{{Name: "bash", Description: "run shell"}}, &fakeExecutor{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply.Content != "the answer" {
		t.Errorf("Content = %q", reply.Content)
	}
	if send.calls != 1 {
		t.Errorf("sender called %d times, want 1 (no extra round for a silent note)", send.calls)
	}
	if got := silentNotesIn(a.History.Snapshot()); len(got) != 0 {
		t.Errorf("late note leaked into history: %q", got)
	}
}

// TestInbox_EnqueueSilent_ConcurrentNoLoss guards the only concurrency the
// feature introduces: the HTTP handler enqueues from a request goroutine while
// the loop drains from the turn goroutine. Run under -race where available.
func TestInbox_EnqueueSilent_ConcurrentNoLoss(t *testing.T) {
	const writers, each = 8, 50
	var ib Inbox
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ib.EnqueueSilent("note")
			}
		}(w)
	}
	// Concurrent drains must never lose an item across the two sides.
	var drained int
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				drained += len(ib.Drain())
				return
			default:
				drained += len(ib.Drain())
			}
		}
	}()
	wg.Wait()
	close(done)
	drained += len(ib.Drain())
	if drained != writers*each {
		t.Fatalf("drained %d notes, want %d", drained, writers*each)
	}
}

// orphanedToolUses returns the ids of tool_use blocks with no tool_result in the
// message that immediately follows their assistant message.
func orphanedToolUses(msgs []Message) []string {
	var out []string
	for i, m := range msgs {
		if m.Role != RoleAssistant {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type != "tool_use" {
				continue
			}
			found := false
			if i+1 < len(msgs) {
				for _, nb := range msgs[i+1].Blocks {
					if nb.Type == "tool_result" && nb.ToolUseID == b.ID {
						found = true
						break
					}
				}
			}
			if !found {
				out = append(out, b.ID)
			}
		}
	}
	return out
}
