package copysync

import (
	"context"
	"fmt"
	"nofx/trader"
)

// CallbackExecutor 允许外部注入执行逻辑，便于逐步接线交易所适配器。
type CallbackExecutor struct {
	Callback func(ctx context.Context, decision *CopyDecision) error
}

func (e *CallbackExecutor) ExecuteCopy(ctx context.Context, decision *CopyDecision) error {
	if e.Callback == nil {
		return fmt.Errorf("copysync: executor callback not set")
	}
	return e.Callback(ctx, decision)
}

// TraderEquityAccount 使用 trader.Trader 的 GetBalance 估算净值（USDT）。
type TraderEquityAccount struct {
	Trader trader.Trader
}

func (a *TraderEquityAccount) GetEquity(ctx context.Context) (float64, error) {
	if a.Trader == nil {
		return 0, fmt.Errorf("copysync: trader is nil")
	}
	bal, err := a.Trader.GetBalance()
	if err != nil {
		return 0, err
	}
	// 兼容多个字段名
	keys := []string{"total_equity", "totalWalletBalance", "wallet_balance", "totalEq", "balance"}
	for _, k := range keys {
		if v, ok := bal[k].(float64); ok && v > 0 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("copysync: equity not found in balance")
}

