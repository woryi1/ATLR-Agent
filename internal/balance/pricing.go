package balance

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// pricingSourceURL is the page every number below was read from. The CNY
// (zh-cn) page is the one that matters: /user/balance reports CNY, and mixing
// in the USD numbers of the English page would be off by ~7×.
const pricingSourceURL = "https://api-docs.deepseek.com/zh-cn/quick_start/pricing"

// Pricing is the DeepSeek price block of /api/balance. Rates are CNY per one
// million tokens and stay strings so the wire format matches the frontend
// contract exactly ("2", not "2.00").
type PriceBlock struct {
	Period         string `json:"period"`           // "peak" | "off_peak"
	InputCacheHit  string `json:"input_cache_hit"`  // 1M input tokens, cache hit
	InputCacheMiss string `json:"input_cache_miss"` // 1M input tokens, cache miss
	Output         string `json:"output"`           // 1M output tokens
	NextSwitchAt   string `json:"next_switch_at"`   // RFC3339, +08:00
	SourceURL      string `json:"source_url"`
}

type rate struct {
	hit    float64
	miss   float64
	output float64
}

type priceTier struct {
	// match is a lowercase fragment of the model name.
	match   string
	peak    rate
	offPeak rate
}

// deepseekRates is the published table, verified 2026-11-13 against
// https://api-docs.deepseek.com/zh-cn/quick_start/pricing (CNY / 1M tokens):
//
//	model             period    cache hit  cache miss  output
//	deepseek-flash    peak      0.04       2.00        8.00
//	deepseek-flash    off-peak  0.02       1.00        4.00
//	deepseek-v4-pro   peak      0.30       9.00        27.00
//	deepseek-v4-pro   off-peak  0.15       4.50        13.50
//
// Off-peak is exactly half of peak, which is the documented rule, not an
// independent measurement. Peak = Beijing time Mon–Fri 09:00–12:00 and
// 14:00–18:00; everything else (including weekends) is off-peak.
//
// Models the page does not list — deepseek-chat, deepseek-reasoner, any
// third-party "deepseek/..." id — have no prices here on purpose. Guessing
// them would show a wrong number, so Pricing returns nil instead.
var deepseekRates = []priceTier{
	{
		match:   "flash",
		peak:    rate{hit: 0.04, miss: 2, output: 8},
		offPeak: rate{hit: 0.02, miss: 1, output: 4},
	},
	{
		match:   "pro",
		peak:    rate{hit: 0.30, miss: 9, output: 27},
		offPeak: rate{hit: 0.15, miss: 4.5, output: 13.5},
	},
}

// Pricing returns the price block for (provider, model, now), or nil when the
// provider has no time-of-day pricing (MiniMax) or the model is not in the
// verified table. nil means "no price to show", never "free".
func Pricing(provider, model string, now time.Time) *PriceBlock {
	if NormalizeProvider(provider) != ProviderDeepSeek {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(model))
	var tier *priceTier
	for i := range deepseekRates {
		if strings.Contains(name, deepseekRates[i].match) {
			tier = &deepseekRates[i]
			break
		}
	}
	if tier == nil {
		return nil
	}

	at := periodAt(now)
	rates := tier.offPeak
	periodName := "off_peak"
	if at.peak {
		rates = tier.peak
		periodName = "peak"
	}
	return &PriceBlock{
		Period:         periodName,
		InputCacheHit:  formatRate(rates.hit),
		InputCacheMiss: formatRate(rates.miss),
		Output:         formatRate(rates.output),
		NextSwitchAt:   at.next.Format(time.RFC3339),
		SourceURL:      pricingSourceURL,
	}
}

// formatRate renders a rate without trailing zeros so 2.00 becomes "2" and
// 0.04 stays "0.04".
func formatRate(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ─── peak / off-peak ─────────────────────────────────────────────────────────

// beijing is the timezone the published windows are defined in. It is loaded
// by name rather than taken from the machine because the rule is about Beijing
// wall-clock time, not about where this process happens to run; the fixed
// fallback covers Go builds without tzdata (no DST in China either way).
var (
	beijingOnce sync.Once
	beijingLoc  *time.Location
)

func beijing() *time.Location {
	beijingOnce.Do(func() {
		loc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			loc = time.FixedZone("CST", 8*3600)
		}
		beijingLoc = loc
	})
	return beijingLoc
}

type period struct {
	peak bool
	next time.Time // the next peak <-> off-peak boundary, in Beijing time
}

// periodAt classifies now and computes the next switch. Only weekends are
// excluded here: Chinese public holidays are off-peak all day per the docs, but
// this build carries no holiday calendar, so a holiday weekday reads as peak
// (the label may be one notch high; no charge depends on it).
func periodAt(now time.Time) period {
	t := now.In(beijing())
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return period{peak: false, next: nextWeekdayPeak(t)}
	}
	switch h := t.Hour(); {
	case h < 9:
		return period{peak: false, next: sameDayAt(t, 9)}
	case h < 12:
		return period{peak: true, next: sameDayAt(t, 12)}
	case h < 14:
		return period{peak: false, next: sameDayAt(t, 14)}
	case h < 18:
		return period{peak: true, next: sameDayAt(t, 18)}
	default:
		return period{peak: false, next: nextWeekdayPeak(t)}
	}
}

func sameDayAt(t time.Time, hour int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), hour, 0, 0, 0, beijing())
}

// nextWeekdayPeak returns the next weekday 09:00 strictly after t.
func nextWeekdayPeak(t time.Time) time.Time {
	day := t.AddDate(0, 0, 1)
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, 1)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, beijing())
}
