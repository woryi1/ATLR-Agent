package agent

import (
	"strings"
	"sync"
)

// InboxItem is one queued user message, optionally carrying content blocks
// (e.g. images pasted in the TUI) that should ride on the same turn.
type InboxItem struct {
	Text   string
	Blocks []ContentBlock

	// Silent marks a per-turn note the user asked to slip into the running
	// turn without interrupting it ("silent send"). The loop injects it at the
	// usual boundary — after the tool batch, before the next provider call —
	// but marks it in the text and never echoes it to the UI as user speech.
	// Items still queued when the turn ends are discarded, not chained into a
	// follow-up turn: a silent note belongs to the turn it was typed into.
	Silent bool
}

// Inbox is a thread-safe queue for user messages that arrive while a turn is
// running. It mirrors Ruby octo's @inbox: messages accumulate here and are
// drained into history at the start of each loop iteration, before the LLM
// call. This keeps mid-turn input handling simple and avoids the complexity of
// merging steer text into tool_result messages.
type Inbox struct {
	mu    sync.Mutex
	items []InboxItem
}

// Enqueue adds a text-only message to the inbox. Empty/whitespace-only
// messages are ignored. Safe to call from any goroutine.
func (ib *Inbox) Enqueue(msg string) {
	ib.EnqueueWithBlocks(msg, nil)
}

// EnqueueWithBlocks adds a message with optional content blocks to the inbox.
// Empty/whitespace-only text is ignored, but a non-empty block list with empty
// text is accepted (image-only steer). Safe to call from any goroutine.
func (ib *Inbox) EnqueueWithBlocks(msg string, blocks []ContentBlock) {
	if strings.TrimSpace(msg) == "" && len(blocks) == 0 {
		return
	}
	ib.mu.Lock()
	ib.items = append(ib.items, InboxItem{Text: msg, Blocks: blocks})
	ib.mu.Unlock()
}

// EnqueueSilent adds a text-only "silent send" note: the running turn appends
// it to history at the next tool boundary, marked so the model reads it as an
// aside, without steering, interrupting or starting a turn. Empty/whitespace-
// only text is ignored. Safe to call from any goroutine.
func (ib *Inbox) EnqueueSilent(msg string) {
	if strings.TrimSpace(msg) == "" {
		return
	}
	ib.mu.Lock()
	ib.items = append(ib.items, InboxItem{Text: msg, Silent: true})
	ib.mu.Unlock()
}

// Drain returns all queued items and clears the inbox. Returns nil when
// nothing is queued. Called from the loop goroutine at iteration start.
func (ib *Inbox) Drain() []InboxItem {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	if len(ib.items) == 0 {
		return nil
	}
	out := ib.items
	ib.items = nil
	return out
}

// HasPending reports whether any messages are queued.
func (ib *Inbox) HasPending() bool {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return len(ib.items) > 0
}

// HasPendingSteer reports whether a real steer message is queued. Silent notes
// do not count: steering is what keeps a turn alive past its final answer, and
// a silent note must never extend a turn — it rides a round the turn was
// already going to make, or it is dropped with the turn.
func (ib *Inbox) HasPendingSteer() bool {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	for _, it := range ib.items {
		if !it.Silent {
			return true
		}
	}
	return false
}

// Pending reports how many messages are queued, without draining them. Used by
// the silent-message endpoint to tell the caller how deep the running turn's
// queue is.
func (ib *Inbox) Pending() int {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return len(ib.items)
}

// Remove deletes the last queued item whose text equals msg and reports
// whether one was removed. It is used to retract a steer message that hasn't
// been drained yet: matching by value (last occurrence) means a background
// notice enqueued between submit and retract doesn't shift the target. Returns
// false when the message is no longer queued (the loop already drained it) —
// the caller must then treat it as committed, not retractable.
func (ib *Inbox) Remove(msg string) bool {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	for i := len(ib.items) - 1; i >= 0; i-- {
		if ib.items[i].Text == msg {
			ib.items = append(ib.items[:i], ib.items[i+1:]...)
			return true
		}
	}
	return false
}

// Texts returns the text of each item, preserving order. Helper for callers
// that only need the string slice (e.g. background notifications).
func Texts(items []InboxItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}
