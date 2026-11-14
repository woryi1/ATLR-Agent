package balance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cst is the input timezone for the period tests. Fixed at +8 so the tests do
// not depend on the machine (or on tzdata being installed).
var cst = time.FixedZone("CST", 8*3600)

func beijingTime(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, cst)
}

// ─── DeepSeek ────────────────────────────────────────────────────────────────

func TestQueryDeepSeek(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"is_available": true,
			"balance_infos": [
				{"currency": "USD", "total_balance": "0.80", "granted_balance": "0.00", "topped_up_balance": "0.80"},
				{"currency": "CNY", "total_balance": "5.81", "granted_balance": "1.20", "topped_up_balance": "4.61"}
			]
		}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), DeepSeekURL: srv.URL}
	got, err := c.Query(context.Background(), ProviderDeepSeek, "sk-test")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if got.Currency != "CNY" {
		t.Errorf("currency = %q, want CNY (the CNY row wins over USD)", got.Currency)
	}
	if got.Plan != nil {
		t.Errorf("plan = %v, want nil for DeepSeek", *got.Plan)
	}
	want := Amounts{Available: "5.81", Cash: "5.81", Granted: "1.20", ToppedUp: "4.61", Owed: "0.00"}
	if *got.Balance != want {
		t.Errorf("balance = %+v, want %+v", *got.Balance, want)
	}
}

func TestQueryDeepSeekHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Authentication Fails"}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), DeepSeekURL: srv.URL}
	_, err := c.Query(context.Background(), ProviderDeepSeek, "sk-test")
	if err == nil {
		t.Fatal("Query: want error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "Authentication Fails") {
		t.Errorf("err = %q, want status and vendor message", err)
	}
}

// ─── MiniMax ─────────────────────────────────────────────────────────────────

func TestQueryMiniMax(t *testing.T) {
	var planCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/account/query_balance", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mm-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{
			"available_amount":"5.81","cash_balance":"5.81","voucher_balance":"2.00",
			"credit_balance":"0.00","owed_amount":"0.30","base_resp":{"status_code":0,"status_msg":"success"}
		}`))
	})
	// The website-level plan endpoint fails, forcing the second level.
	mux.HandleFunc("/backend/account/token_plan/remains_percent", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&planCalls, 1)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/v1/token_plan/remains", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&planCalls, 1)
		_, _ = w.Write([]byte(`{
			"model_remains":[{"model_name":"MiniMax-M3","current_interval_used_count":1200,"current_interval_total_count":5000}],
			"base_resp":{"status_code":0}
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{
		HTTP:              srv.Client(),
		MiniMaxBalanceURL: srv.URL + "/account/query_balance",
		MiniMaxPlanURLs: []string{
			srv.URL + "/backend/account/token_plan/remains_percent",
			srv.URL + "/v1/token_plan/remains",
		},
	}
	got, err := c.Query(context.Background(), ProviderMiniMax, "mm-test")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Currency != "CNY" {
		t.Errorf("currency = %q, want CNY", got.Currency)
	}
	want := Amounts{Available: "5.81", Cash: "5.81", Granted: "2.00", ToppedUp: "", Owed: "0.30"}
	if *got.Balance != want {
		t.Errorf("balance = %+v, want %+v", *got.Balance, want)
	}
	if got.Plan == nil || *got.Plan != "MiniMax-M3 1200/5000" {
		t.Errorf("plan = %v, want MiniMax-M3 1200/5000", got.Plan)
	}
	if n := atomic.LoadInt32(&planCalls); n != 2 {
		t.Errorf("plan endpoint calls = %d, want 2 (primary then fallback)", n)
	}
}

func TestQueryMiniMaxNoPlan(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"available_amount":"1.00","base_resp":{"status_code":0}}`))
	})
	mux.HandleFunc("/plan", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"base_resp":{"status_code":2062}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{
		HTTP:              srv.Client(),
		MiniMaxBalanceURL: srv.URL + "/balance",
		MiniMaxPlanURLs:   []string{srv.URL + "/plan"},
	}
	got, err := c.Query(context.Background(), ProviderMiniMax, "mm-test")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Plan == nil || *got.Plan != planNone {
		t.Errorf("plan = %v, want %q for status_code 2062", got.Plan, planNone)
	}
}

func TestQueryMiniMaxRedactsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"invalid key sk-secret-value"}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), MiniMaxBalanceURL: srv.URL}
	_, err := c.Query(context.Background(), ProviderMiniMax, "sk-secret-value")
	if err == nil {
		t.Fatal("Query: want error, got nil")
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Errorf("err = %q leaks the API key", err)
	}
}

// ─── provider / key guards ───────────────────────────────────────────────────

func TestQueryNoAPIKey(t *testing.T) {
	c := NewClient()
	_, err := c.Query(context.Background(), ProviderDeepSeek, "")
	if err != ErrNoAPIKey {
		t.Errorf("err = %v, want ErrNoAPIKey", err)
	}
}

func TestUnsupportedProviders(t *testing.T) {
	for _, provider := range []string{"openrouter", "custom", "openai", "", "anthropic"} {
		if Supported(provider) {
			t.Errorf("Supported(%q) = true, want false", provider)
		}
		if _, err := NewClient().Query(context.Background(), provider, "sk-test"); err == nil {
			t.Errorf("Query(%q) = nil error, want ErrUnsupportedProvider", provider)
		} else if !strings.Contains(err.Error(), ErrUnsupportedProvider.Error()) {
			t.Errorf("Query(%q) err = %v, want ErrUnsupportedProvider", provider, err)
		}
	}
	for _, provider := range []string{"deepseek", "DeepSeek ", " minimax"} {
		if !Supported(provider) {
			t.Errorf("Supported(%q) = false, want true", provider)
		}
	}
}

// ─── pricing / periods ───────────────────────────────────────────────────────

func TestPricingPeriods(t *testing.T) {
	cases := []struct {
		name   string
		at     time.Time
		period string
		next   string
	}{
		{"08:59", beijingTime(2026, time.November, 13, 8, 59), "off_peak", "2026-11-13T09:00:00+08:00"},
		{"09:00", beijingTime(2026, time.November, 13, 9, 0), "peak", "2026-11-13T12:00:00+08:00"},
		{"11:59", beijingTime(2026, time.November, 13, 11, 59), "peak", "2026-11-13T12:00:00+08:00"},
		{"12:00", beijingTime(2026, time.November, 13, 12, 0), "off_peak", "2026-11-13T14:00:00+08:00"},
		{"13:59", beijingTime(2026, time.November, 13, 13, 59), "off_peak", "2026-11-13T14:00:00+08:00"},
		{"14:00", beijingTime(2026, time.November, 13, 14, 0), "peak", "2026-11-13T18:00:00+08:00"},
		{"17:59", beijingTime(2026, time.November, 13, 17, 59), "peak", "2026-11-13T18:00:00+08:00"},
		{"18:00", beijingTime(2026, time.November, 13, 18, 0), "off_peak", "2026-11-16T09:00:00+08:00"},
		{"saturday", beijingTime(2026, time.November, 14, 10, 0), "off_peak", "2026-11-16T09:00:00+08:00"},
		{"sunday", beijingTime(2026, time.November, 15, 23, 0), "off_peak", "2026-11-16T09:00:00+08:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Pricing(ProviderDeepSeek, "deepseek-flash", tc.at)
			if got == nil {
				t.Fatal("Pricing returned nil")
			}
			if got.Period != tc.period {
				t.Errorf("period = %q, want %q", got.Period, tc.period)
			}
			if got.NextSwitchAt != tc.next {
				t.Errorf("next_switch_at = %q, want %q", got.NextSwitchAt, tc.next)
			}
		})
	}
}

func TestPricingRates(t *testing.T) {
	peak := Pricing(ProviderDeepSeek, "deepseek-flash", beijingTime(2026, time.November, 13, 10, 0))
	if peak == nil {
		t.Fatal("flash peak: nil")
	}
	if peak.InputCacheHit != "0.04" || peak.InputCacheMiss != "2" || peak.Output != "8" {
		t.Errorf("flash peak rates = %s/%s/%s, want 0.04/2/8", peak.InputCacheHit, peak.InputCacheMiss, peak.Output)
	}
	if peak.SourceURL != pricingSourceURL {
		t.Errorf("source_url = %q, want %q", peak.SourceURL, pricingSourceURL)
	}

	off := Pricing(ProviderDeepSeek, "deepseek-v4-pro", beijingTime(2026, time.November, 13, 20, 0))
	if off == nil {
		t.Fatal("pro off-peak: nil")
	}
	if off.Period != "off_peak" || off.InputCacheHit != "0.15" || off.InputCacheMiss != "4.5" || off.Output != "13.5" {
		t.Errorf("pro off-peak = %+v, want 0.15/4.5/13.5", off)
	}
}

func TestPricingUnknown(t *testing.T) {
	if got := Pricing(ProviderDeepSeek, "deepseek-chat", time.Now()); got != nil {
		t.Errorf("Pricing(deepseek-chat) = %+v, want nil (no verified price)", got)
	}
	if got := Pricing(ProviderMiniMax, "MiniMax-M3", time.Now()); got != nil {
		t.Errorf("Pricing(minimax) = %+v, want nil", got)
	}
	if got := Pricing("openrouter", "deepseek/deepseek-v4-flash", time.Now()); got != nil {
		t.Errorf("Pricing(openrouter) = %+v, want nil", got)
	}
}

// ─── cache ───────────────────────────────────────────────────────────────────

func fixedDeepSeekServer(t *testing.T, calls *int32, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

const deepSeekOKBody = `{"balance_infos":[{"currency":"CNY","total_balance":"5.81","granted_balance":"0.00","topped_up_balance":"5.81"}]}`

func TestCacheTTL(t *testing.T) {
	var calls int32
	srv := fixedDeepSeekServer(t, &calls, deepSeekOKBody, http.StatusOK)
	defer srv.Close()

	now := beijingTime(2026, time.November, 13, 10, 0)
	cache := NewCache(&Client{HTTP: srv.Client(), DeepSeekURL: srv.URL})
	cache.now = func() time.Time { return now }

	first, err := cache.Get(context.Background(), ProviderDeepSeek, "sk-test", false)
	if err != nil || first.Cached {
		t.Fatalf("first Get = %+v, %v; want fresh", first, err)
	}

	// Inside the 10s cooldown, ?refresh=1 is served from cache too.
	now = now.Add(5 * time.Second)
	second, err := cache.Get(context.Background(), ProviderDeepSeek, "sk-test", true)
	if err != nil || !second.Cached {
		t.Fatalf("refresh inside cooldown = %+v, %v; want cache hit", second, err)
	}

	// A plain read inside the TTL is a cache hit as well.
	now = now.Add(25 * time.Second)
	third, err := cache.Get(context.Background(), ProviderDeepSeek, "sk-test", false)
	if err != nil || !third.Cached {
		t.Fatalf("read inside TTL = %+v, %v; want cache hit", third, err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("vendor calls = %d, want 1", n)
	}

	// Past the cooldown but inside the TTL, refresh bypasses the TTL.
	now = now.Add(10 * time.Second)
	fourth, err := cache.Get(context.Background(), ProviderDeepSeek, "sk-test", true)
	if err != nil || fourth.Cached {
		t.Fatalf("refresh past cooldown = %+v, %v; want fresh", fourth, err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("vendor calls = %d, want 2", n)
	}

	// Past the TTL, a normal read refetches.
	now = now.Add(61 * time.Second)
	if _, err := cache.Get(context.Background(), ProviderDeepSeek, "sk-test", false); err != nil {
		t.Fatalf("Get after TTL: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("vendor calls = %d, want 3", n)
	}
}

func TestCacheStaleFallback(t *testing.T) {
	var calls int32
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(deepSeekOKBody))
	}))
	defer srv.Close()

	now := beijingTime(2026, time.November, 13, 10, 0)
	cache := NewCache(&Client{HTTP: srv.Client(), DeepSeekURL: srv.URL})
	cache.now = func() time.Time { return now }

	first, err := cache.Get(context.Background(), ProviderDeepSeek, "sk-test", false)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}

	failing.Store(true)

	// TTL expired, vendor down: the old value is served and labelled stale.
	now = now.Add(2 * time.Minute)
	stale, err := cache.Get(context.Background(), ProviderDeepSeek, "sk-test", false)
	if err == nil {
		t.Fatal("stale Get: want the failure to be reported alongside the value")
	}
	if !stale.Cached || !stale.Stale {
		t.Errorf("snapshot = %+v, want cached+stale", stale)
	}
	if stale.Result.Balance == nil || stale.Result.Balance.Available != first.Result.Balance.Available {
		t.Errorf("stale balance = %+v, want the previous value", stale.Result.Balance)
	}

	// Beyond the 10 minute window nothing is served.
	now = now.Add(9 * time.Minute)
	gone, err := cache.Get(context.Background(), ProviderDeepSeek, "sk-test", false)
	if err == nil {
		t.Fatal("expired Get: want error")
	}
	if gone.Result.Balance != nil {
		t.Errorf("snapshot = %+v, want empty after the stale window", gone)
	}
}

func TestCacheKeysDifferByKey(t *testing.T) {
	var calls int32
	srv := fixedDeepSeekServer(t, &calls, deepSeekOKBody, http.StatusOK)
	defer srv.Close()

	cache := NewCache(&Client{HTTP: srv.Client(), DeepSeekURL: srv.URL})
	if _, err := cache.Get(context.Background(), ProviderDeepSeek, "key-one", false); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(context.Background(), ProviderDeepSeek, "key-two", false); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("vendor calls = %d, want 2 (different keys must not share a cache slot)", n)
	}
}

// TestResponseShapeJSON pins the wire names the frontend reads. A rename here
// is a silent UI regression, so it is asserted rather than assumed.
func TestResponseShapeJSON(t *testing.T) {
	block := Pricing(ProviderDeepSeek, "deepseek-flash", beijingTime(2026, time.November, 13, 10, 0))
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"period"`, `"input_cache_hit"`, `"input_cache_miss"`, `"output"`, `"next_switch_at"`, `"source_url"`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("pricing JSON %s misses %s", raw, field)
		}
	}

	amounts, err := json.Marshal(Amounts{Available: "1", Cash: "1", Granted: "0", ToppedUp: "0", Owed: "0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"available"`, `"cash"`, `"granted"`, `"topped_up"`, `"owed"`} {
		if !strings.Contains(string(amounts), field) {
			t.Errorf("balance JSON %s misses %s", amounts, field)
		}
	}
}
