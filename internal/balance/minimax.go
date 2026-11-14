package balance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// MiniMax token-plan labels, copied from the reference script. "未查询到" is
// used when the balance read worked but neither plan endpoint answered — it is
// a fact about the query, not about the account.
const (
	planNone    = "无生效中的套餐订阅"
	planUnknown = "未查询到"

	// planNoSubscription is MiniMax's status_code for "this account has no
	// token plan", which is a normal state, not an error.
	planNoSubscription = 2062
)

type miniMaxBaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// miniMaxBalance is GET https://www.minimaxi.com/account/query_balance.
type miniMaxBalance struct {
	AvailableAmount json.Number     `json:"available_amount"`
	CashBalance     json.Number     `json:"cash_balance"`
	VoucherBalance  json.Number     `json:"voucher_balance"`
	OwedAmount      json.Number     `json:"owed_amount"`
	BaseResp        miniMaxBaseResp `json:"base_resp"`
}

// miniMaxPlan is the token-plan quota: /backend/account/token_plan/remains_percent
// or /v1/token_plan/remains. Both answer with a model_remains array when a plan
// is active.
type miniMaxPlan struct {
	ModelRemains []struct {
		ModelName                 string      `json:"model_name"`
		Model                     string      `json:"model"`
		CurrentIntervalUsedCount  json.Number `json:"current_interval_used_count"`
		CurrentIntervalTotalCount json.Number `json:"current_interval_total_count"`
	} `json:"model_remains"`
	BaseResp miniMaxBaseResp `json:"base_resp"`
}

func (c *Client) queryMiniMax(ctx context.Context, key string) (Result, error) {
	var body miniMaxBalance
	if err := c.fetch(ctx, c.MiniMaxBalanceURL, key, &body); err != nil {
		return Result{}, err
	}
	if body.BaseResp.StatusCode != 0 {
		msg := body.BaseResp.StatusMsg
		if msg == "" {
			msg = fmt.Sprintf("status_code %d", body.BaseResp.StatusCode)
		}
		return Result{}, errors.New(sanitize(msg, key))
	}
	if body.AvailableAmount == "" {
		return Result{}, errors.New("返回里没有 available_amount")
	}

	plan := c.miniMaxPlan(ctx, key)
	return Result{
		Currency: CurrencyCNY,
		Balance: &Amounts{
			Available: money(body.AvailableAmount),
			Cash:      money(body.CashBalance),
			// Vouchers are the closest thing MiniMax has to granted credit.
			Granted: money(body.VoucherBalance),
			// MiniMax reports no separate top-up bucket, so the field stays
			// empty rather than repeating cash.
			ToppedUp: "",
			Owed:     money(body.OwedAmount),
		},
		Plan: &plan,
	}, nil
}

// miniMaxPlan walks the two plan endpoints. A failure of either is not a
// failure of the balance read: the plan is decoration, the balance is the data.
func (c *Client) miniMaxPlan(ctx context.Context, key string) string {
	for _, url := range c.MiniMaxPlanURLs {
		var body miniMaxPlan
		if err := c.fetch(ctx, url, key, &body); err != nil {
			continue
		}
		switch body.BaseResp.StatusCode {
		case 0:
			if len(body.ModelRemains) == 0 {
				return planNone
			}
			q := body.ModelRemains[0]
			name := q.ModelName
			if name == "" {
				name = q.Model
			}
			if name == "" {
				name = "套餐"
			}
			return fmt.Sprintf("%s %s/%s", name, q.CurrentIntervalUsedCount.String(), q.CurrentIntervalTotalCount.String())
		case planNoSubscription:
			return planNone
		}
	}
	return planUnknown
}
