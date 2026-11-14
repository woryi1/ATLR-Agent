package balance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// deepSeekBalance is GET https://api.deepseek.com/user/balance. Amounts arrive
// as strings ("5.81"), but json.Number accepts a bare JSON number too, so a
// vendor-side type change cannot break the parse.
type deepSeekBalance struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string      `json:"currency"`
		TotalBalance    json.Number `json:"total_balance"`
		GrantedBalance  json.Number `json:"granted_balance"`
		ToppedUpBalance json.Number `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

func (c *Client) queryDeepSeek(ctx context.Context, key string) (Result, error) {
	var body deepSeekBalance
	if err := c.fetch(ctx, c.DeepSeekURL, key, &body); err != nil {
		return Result{}, err
	}
	if len(body.BalanceInfos) == 0 {
		return Result{}, errors.New("返回里没有 balance_infos")
	}

	// CNY is the account's billing currency here (and the pricing table below
	// is CNY); fall back to the first entry rather than failing when a
	// USD-only account answers.
	info := body.BalanceInfos[0]
	for _, candidate := range body.BalanceInfos {
		if strings.EqualFold(candidate.Currency, CurrencyCNY) {
			info = candidate
			break
		}
	}
	if info.TotalBalance == "" {
		return Result{}, errors.New("返回里没有 total_balance")
	}

	currency := strings.ToUpper(strings.TrimSpace(info.Currency))
	if currency == "" {
		currency = CurrencyCNY
	}
	total := money(info.TotalBalance)
	return Result{
		Currency: currency,
		Balance: &Amounts{
			Available: total,
			// DeepSeek has no separate "cash" figure: the whole balance is
			// spendable, so the two mirror each other.
			Cash:     total,
			Granted:  money(info.GrantedBalance),
			ToppedUp: money(info.ToppedUpBalance),
			// DeepSeek has no credit line, so nothing can be owed.
			Owed: "0.00",
		},
	}, nil
}
