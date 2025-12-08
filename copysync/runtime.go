package copysync

import (
	"fmt"
	"nofx/market"
	"nofx/trader"
)

// NewServiceForTrader 根据 CopyConfig 与 provider 选择创建 CopySync Service。
// followerTrader：跟随账户的交易适配器；用于取净值与下单。
func NewServiceForTrader(cfg CopyConfig, followerTrader trader.Trader, traderID string, orderLogger func(o *store.TraderOrder)) (*Service, error) {
	cfg.EnsureDefaults()

	// 创建 provider
	var provider Provider
	switch cfg.ProviderType {
	case "okx_wallet":
		uniqueName := ""
		if cfg.ProviderParams != nil {
			uniqueName = cfg.ProviderParams["uniqueName"]
		}
		if uniqueName == "" {
			return nil, fmt.Errorf("copysync: okx_wallet requires provider_params.uniqueName")
		}
		provider = NewOKXProvider(uniqueName)
	case "hl_wallet", "hyperliquid_wallet":
		addr := ""
		if cfg.ProviderParams != nil {
			addr = cfg.ProviderParams["address"]
		}
		if addr == "" {
			return nil, fmt.Errorf("copysync: hyperliquid_wallet requires provider_params.address")
		}
		provider = NewHyperliquidProvider(addr)
	default:
		return nil, fmt.Errorf("copysync: unsupported provider_type %s", cfg.ProviderType)
	}

	// 跟随账户净值
	account := &TraderEquityAccount{Trader: followerTrader}

	// 执行器
	exec := &TraderExecutor{
		Trader:             followerTrader,
		Config:             cfg,
		EnableLeverageSync: cfg.LeverageSync,
		EnableMarginSync:   cfg.MarginModeSync,
	}
	exec.OrderLogger = func(o *store.TraderOrder) {
		if orderLogger == nil || o == nil {
			return
		}
		o.TraderID = traderID
		orderLogger(o)
	}

	// 行情兜底价
	priceFunc := func(symbol string) (float64, error) {
		data, err := market.Get(symbol)
		if err != nil {
			return 0, err
		}
		return data.CurrentPrice, nil
	}

	service := NewService(cfg, provider, account, exec, priceFunc)
	return service, nil
}
