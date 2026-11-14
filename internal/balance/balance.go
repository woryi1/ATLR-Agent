// Package balance fetches the account balance a provider exposes over its own
// HTTP API (DeepSeek, MiniMax) and derives DeepSeek's peak/off-peak price
// table. It knows nothing about sessions or config: the caller resolves the
// provider and the API key first, then hands them here.
package balance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Providers with a balance endpoint of their own. Anything else — including an
// OpenRouter entry that serves a "deepseek/..." model — must never be queried:
// that key belongs to a different vendor, so sending it to api.deepseek.com
// would both fail and hand one vendor's credential to another vendor's domain.
const (
	ProviderDeepSeek = "deepseek"
	ProviderMiniMax  = "minimax"

	CurrencyCNY = "CNY"
)

// Errors callers map onto the response body (never onto an HTTP status: the
// balance endpoint answers 200 with the failure in `error`).
var (
	ErrNoAPIKey            = errors.New("未配置 API Key")
	ErrUnsupportedProvider = errors.New("该 provider 没有余额查询接口")
)

// Amounts is the provider-neutral balance projection. Values stay strings
// because that is how both providers report them ("5.81") and because the
// frontend renders them as-is. A field the provider does not report is "".
type Amounts struct {
	Available string `json:"available"`
	Cash      string `json:"cash"`
	Granted   string `json:"granted"`
	ToppedUp  string `json:"topped_up"`
	Owed      string `json:"owed"`
}

// Result is one successful balance read. Plan is MiniMax's token-plan label
// ("MiniMax-M3 1200/5000", or "无生效中的套餐订阅" when none is active);
// DeepSeek reports no plan, so Plan is nil there.
type Result struct {
	Currency string
	Balance  *Amounts
	Plan     *string
}

// NormalizeProvider lower-cases and trims a config provider id. The registry
// has no aliases (app.vendorByID matches by equality), so this only absorbs
// case and stray whitespace.
func NormalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// Supported reports whether the provider has a balance API this package can
// talk to. Callers must check this before resolving a key so a non-supported
// provider never has its key sent anywhere.
func Supported(provider string) bool {
	switch NormalizeProvider(provider) {
	case ProviderDeepSeek, ProviderMiniMax:
		return true
	}
	return false
}

// Client talks to the two balance endpoints. The zero-value defaults are set
// by NewClient; the URL fields are exported so tests can point them at an
// httptest server.
type Client struct {
	// HTTP defaults to a client with a 6s timeout. The model picker opens on
	// a click, so a slow vendor must fail fast instead of freezing the panel.
	HTTP *http.Client

	DeepSeekURL       string
	MiniMaxBalanceURL string
	MiniMaxPlanURLs   []string
}

// NewClient returns a Client wired to the production endpoints.
func NewClient() *Client {
	return &Client{
		HTTP:              &http.Client{Timeout: 6 * time.Second},
		DeepSeekURL:       "https://api.deepseek.com/user/balance",
		MiniMaxBalanceURL: "https://www.minimaxi.com/account/query_balance",
		// Two levels, as the reference script does it: the website endpoint
		// covers the mainland console, the api host covers keys that only the
		// open-platform edge knows about.
		MiniMaxPlanURLs: []string{
			"https://www.minimaxi.com/backend/account/token_plan/remains_percent",
			"https://api.minimaxi.com/v1/token_plan/remains",
		},
	}
}

// Query performs one uncached balance read. The provider whitelist is checked
// first and an empty key is rejected without a request, so a key that belongs
// to another vendor is never even read on the way to a rejection.
func (c *Client) Query(ctx context.Context, provider, key string) (Result, error) {
	provider = NormalizeProvider(provider)
	if !Supported(provider) {
		return Result{}, fmt.Errorf("%w: %q", ErrUnsupportedProvider, provider)
	}
	if key == "" {
		return Result{}, ErrNoAPIKey
	}
	switch provider {
	case ProviderDeepSeek:
		return c.queryDeepSeek(ctx, key)
	default:
		return c.queryMiniMax(ctx, key)
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// fetch is an authenticated GET that decodes a 200 body into v. Non-200 bodies
// are parsed for the vendor's own message (DeepSeek returns
// {"error":{"message":...}}) so the UI can show the real reason.
func (c *Client) fetch(ctx context.Context, url, key string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("构建请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %s", sanitize(err.Error(), key))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取响应失败: %s", sanitize(err.Error(), key))
	}
	if resp.StatusCode != http.StatusOK {
		if msg := vendorMessage(body); msg != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, sanitize(msg, key))
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("解析响应失败: %v", err)
	}
	return nil
}

// vendorMessage pulls the human-readable message out of an error body. Both
// vendors nest it differently, so try each shape and give up quietly.
func vendorMessage(body []byte) string {
	var withError struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &withError) == nil && withError.Error.Message != "" {
		return withError.Error.Message
	}
	var withBaseResp struct {
		BaseResp struct {
			StatusMsg string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if json.Unmarshal(body, &withBaseResp) == nil {
		return withBaseResp.BaseResp.StatusMsg
	}
	return ""
}

// sanitize removes the API key (and its leading fragment) from any text that
// may reach a log line or a response body. Vendors occasionally echo a key
// back in an auth error.
func sanitize(msg, key string) string {
	if msg == "" || key == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, key, "[redacted]")
	if len(key) > 8 {
		msg = strings.ReplaceAll(msg, key[:8], "[redacted]")
	}
	return msg
}

// money formats a provider amount the way the reference script does: two
// decimals, empty when the field is absent.
func money(n json.Number) string {
	if n == "" {
		return ""
	}
	f, err := n.Float64()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%.2f", f)
}

// ─── cache ───────────────────────────────────────────────────────────────────

const (
	// cacheTTL is how long a successful read is served without asking again.
	cacheTTL = 60 * time.Second
	// refreshCooldown bounds ?refresh=1: a user mashing the refresh button
	// cannot hit the vendor more than once per 10s per key.
	refreshCooldown = 10 * time.Second
	// staleWindow is how long an expired value may be served after a failure.
	staleWindow = 10 * time.Minute
)

// Snapshot is a cache read: the value plus how it was obtained.
type Snapshot struct {
	Result Result
	// Cached is true when the value came from the cache instead of a fresh
	// request (including a stale replay).
	Cached bool
	// Stale is true when a request failed and an older value was served.
	Stale bool
}

type cacheEntry struct {
	result Result
	at     time.Time
}

// Cache memoises reads per provider+key fingerprint. One instance is enough
// per process; see Default.
type Cache struct {
	client *Client

	mu      sync.Mutex
	entries map[string]cacheEntry

	// now is the clock; tests replace it to drive the TTL.
	now func() time.Time
}

// NewCache wraps client with the process cache.
func NewCache(client *Client) *Cache {
	return &Cache{client: client, entries: make(map[string]cacheEntry), now: time.Now}
}

// Default is the instance the HTTP handler uses. Balance data is keyed by
// provider+key, not by session, so process-wide is the right scope.
var Default = NewCache(NewClient())

// Get returns a balance, from cache when fresh enough. A failed request falls
// back to a value no older than staleWindow and reports it as stale, with the
// failure returned alongside so the caller can surface both.
func (c *Cache) Get(ctx context.Context, provider, key string, refresh bool) (Snapshot, error) {
	provider = NormalizeProvider(provider)
	// Never store the key itself: the fingerprint is enough to tell two keys
	// on the same provider apart.
	cacheKey := provider + "|" + fingerprint(key)

	start := c.now()
	c.mu.Lock()
	old, hit := c.entries[cacheKey]
	c.mu.Unlock()

	if hit {
		// The cooldown applies to ?refresh=1 too: inside 10s a refresh is
		// answered from cache, so holding the button cannot hammer the vendor.
		age := start.Sub(old.at)
		if age < refreshCooldown || (!refresh && age < cacheTTL) {
			return Snapshot{Result: old.result, Cached: true}, nil
		}
	}

	result, err := c.client.Query(ctx, provider, key)
	if err != nil {
		if hit && start.Sub(old.at) <= staleWindow {
			return Snapshot{Result: old.result, Cached: true, Stale: true}, err
		}
		return Snapshot{}, err
	}

	c.mu.Lock()
	c.entries[cacheKey] = cacheEntry{result: result, at: c.now()}
	c.mu.Unlock()
	return Snapshot{Result: result}, nil
}

// fingerprint is the first 8 hex characters of sha256(key) — enough to
// separate keys without keeping the secret in memory beyond the request.
func fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:8]
}
