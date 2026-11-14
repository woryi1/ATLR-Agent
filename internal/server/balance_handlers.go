package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/open-octo/octo-agent/internal/agent"
	"github.com/open-octo/octo-agent/internal/app"
	"github.com/open-octo/octo-agent/internal/balance"
	"github.com/open-octo/octo-agent/internal/config"
)

// balanceResponse is the wire shape of GET /api/balance. The field names are
// the frontend contract — the model picker renders them directly, so renaming
// one silently blanks the balance line.
type balanceResponse struct {
	Supported bool                `json:"supported"`
	Provider  string              `json:"provider"`
	Model     string              `json:"model"`
	Currency  string              `json:"currency"`
	Balance   *balance.Amounts    `json:"balance"`
	Plan      *string             `json:"plan"`
	Pricing   *balance.PriceBlock `json:"pricing"`
	Cached    bool                `json:"cached"`
	Stale     bool                `json:"stale"`
	Error     *string             `json:"error"`
}

// ─── GET /api/balance ────────────────────────────────────────────────────────

// handleGetBalance reports the account balance behind the model the caller is
// looking at. Two ways in: session_id (the picker opened inside a session —
// the session's own binding decides the provider) or endpoint_id+model (the
// landing page, where no session exists yet).
//
// The handler always answers 200 with the failure described in `error`, so the
// picker can render what it has instead of treating a dead vendor as a broken
// request. Only a missing model reference is a 400.
//
// The key is read here and never leaves the process: it is not echoed, not
// logged, and only reaches the vendor's own endpoint for a whitelisted
// provider (see balance.Supported).
func (s *Server) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID := strings.TrimSpace(q.Get("session_id"))
	endpointID := strings.TrimSpace(q.Get("endpoint_id"))
	modelRef := strings.TrimSpace(q.Get("model"))
	refresh := q.Get("refresh") == "1"

	if sessionID == "" && modelRef == "" {
		writeError(w, http.StatusBadRequest, "session_id or model is required")
		return
	}

	cfg, _ := config.Load()
	entry, model, err := resolveBalanceEntry(cfg, sessionID, endpointID, modelRef)
	if err != nil {
		writeJSON(w, http.StatusOK, balanceResponse{Currency: balance.CurrencyCNY, Error: stringPtr(err.Error())})
		return
	}

	// Same rule as senderForEntry (server.go:1874-1877): the vendor's env var
	// first, then the key stored on the entry.
	provider := balance.NormalizeProvider(entry.Provider)
	key := os.Getenv(app.VendorAPIKeyEnvVar(provider))
	if key == "" {
		key = entry.APIKey
	}

	resp := balanceResponse{
		Provider: provider,
		Model:    model,
		Currency: balance.CurrencyCNY,
	}

	// Whitelist first, key second: a provider without a balance API must never
	// receive its key anywhere, and "no API key" would be a misleading reason
	// for, say, an OpenRouter entry.
	if !balance.Supported(provider) {
		reason := fmt.Sprintf("provider %q 没有余额查询接口", provider)
		if provider == "" {
			reason = "该模型未设置 provider，无法查询余额"
		}
		resp.Error = stringPtr(reason)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Supported = true
	if key == "" {
		resp.Supported = false
		resp.Error = stringPtr("该模型未配置 API Key")
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Pricing is local and cheap, so it is computed even if the balance read
	// fails: knowing the period is still useful when the vendor is unreachable.
	if provider == balance.ProviderDeepSeek {
		resp.Pricing = balance.Pricing(provider, model, time.Now())
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	snap, qerr := balance.Default.Get(ctx, provider, key, refresh)
	resp.Cached, resp.Stale = snap.Cached, snap.Stale
	// A stale snapshot carries the last good value alongside the failure.
	if snap.Result.Balance != nil {
		resp.Balance = snap.Result.Balance
		resp.Plan = snap.Result.Plan
		if snap.Result.Currency != "" {
			resp.Currency = snap.Result.Currency
		}
	}
	if qerr != nil {
		resp.Error = stringPtr(qerr.Error())
	} else if provider == balance.ProviderDeepSeek && resp.Pricing == nil {
		// No price table entry: say so instead of showing a guessed number.
		resp.Error = stringPtr(fmt.Sprintf("未收录该模型价格：%s", model))
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveBalanceEntry turns the request's model reference into the config entry
// that actually backs it, mirroring senderForSession's resolution: a session's
// ModelConfig wins, and its own model name is kept when the binding no longer
// resolves.
func resolveBalanceEntry(cfg config.Config, sessionID, endpointID, modelRef string) (config.ModelEntry, string, error) {
	if sessionID != "" {
		sess, err := agent.LoadSession(sessionID)
		if err != nil {
			return config.ModelEntry{}, "", fmt.Errorf("加载会话失败: %v", err)
		}
		entry := entryForSession(cfg, sess)
		model := sess.Model
		if sess.ModelConfig != "" {
			if bound, ok := cfg.EntryByModel(sess.ModelConfig); ok && bound.Model != "" {
				model = bound.Model
			}
		}
		if model == "" {
			model = entry.Model
		}
		return entry, model, nil
	}

	// endpoint_id pins the lookup: falling back to a bare model name could
	// land on a different endpoint that serves the same model, and that
	// endpoint may belong to another vendor with another key.
	if endpointID != "" {
		ref := endpointID + "::" + modelRef
		entry, ok := cfg.EntryByModel(ref)
		if !ok {
			return config.ModelEntry{}, "", fmt.Errorf("未找到模型配置: %s", ref)
		}
		return entry, entry.Model, nil
	}
	entry, ok := cfg.EntryByModel(modelRef)
	if !ok {
		return config.ModelEntry{}, "", fmt.Errorf("未找到模型配置: %s", modelRef)
	}
	return entry, entry.Model, nil
}

func stringPtr(s string) *string { return &s }
