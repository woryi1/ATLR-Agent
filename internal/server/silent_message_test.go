package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-octo/octo-agent/internal/agent"
)

// silentReq builds a POST /api/sessions/{id}/silent-message request with the
// path value filled in, so the handler can be exercised without the router.
func silentReq(t *testing.T, sid, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/silent-message", strings.NewReader(body))
	r.SetPathValue("id", sid)
	return r
}

// TestHandleSilentMessage_NoRunningTurn409 pins the failure contract the web
// UI keys off to tell the user "there is no turn to append to — send normally
// instead".
func TestHandleSilentMessage_NoRunningTurn409(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.sessionAgents = make(map[string]*agent.Agent)

	w := httptest.NewRecorder()
	srv.handleSilentMessage(w, silentReq(t, "sess-none", `{"text":"one more thing"}`))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", w.Body.String())
	}
	if body["error"] == "" {
		t.Errorf("409 body carries no error message: %s", w.Body.String())
	}
}

// TestHandleSilentMessage_QueuesOnRunningTurn covers the happy path: two notes
// land on the registered turn's Inbox, in order, and the response reports the
// queue depth without draining anything.
func TestHandleSilentMessage_QueuesOnRunningTurn(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.sessionAgents = make(map[string]*agent.Agent)
	a := agent.New(nil, "stub-model")
	srv.sessionAgents["sess-live"] = a

	for _, text := range []string{"first", "second"} {
		w := httptest.NewRecorder()
		srv.handleSilentMessage(w, silentReq(t, "sess-live", `{"text":"`+text+`"}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
	}

	var resp struct {
		Queued  bool `json:"queued"`
		Pending int  `json:"pending"`
	}
	w := httptest.NewRecorder()
	srv.handleSilentMessage(w, silentReq(t, "sess-live", `{"text":"third"}`))
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %s", w.Body.String())
	}
	if !resp.Queued || resp.Pending != 3 {
		t.Errorf("response = %+v, want queued:true pending:3", resp)
	}

	items := a.Inbox.Drain()
	if len(items) != 3 {
		t.Fatalf("inbox drained %d items, want 3", len(items))
	}
	for i, want := range []string{"first", "second", "third"} {
		if items[i].Text != want {
			t.Errorf("item[%d] = %q, want %q", i, items[i].Text, want)
		}
		if !items[i].Silent {
			t.Errorf("item[%d] (%q) is not marked Silent", i, items[i].Text)
		}
	}
	// A silent note must never look like a steer: it must not keep a turn alive.
	if a.Inbox.HasPendingSteer() {
		t.Error("silent notes counted as pending steers")
	}
}

// TestHandleSilentMessage_EmptyText400: whitespace-only notes are rejected
// rather than queued as a no-op the model can't act on.
func TestHandleSilentMessage_EmptyText400(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.sessionAgents = make(map[string]*agent.Agent)
	srv.sessionAgents["sess-live"] = agent.New(nil, "stub-model")

	w := httptest.NewRecorder()
	srv.handleSilentMessage(w, silentReq(t, "sess-live", `{"text":"   "}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
