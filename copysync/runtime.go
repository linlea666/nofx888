package copysync

import (
	"encoding/json"
	"fmt"
	"nofx/market"
	"nofx/store"
	"nofx/trader"
	"strconv"
)

// NewServiceForTrader 根据 CopyConfig 与 provider 选择创建 CopySync Service。
// followerTrader：跟随账户的交易适配器；用于取净值与下单。
func NewServiceForTrader(cfg CopyConfig, followerTrader trader.Trader, traderID string, orderLogger func(o *store.TraderOrder, dec *CopyDecision, execErr error)) (*Service, error) {
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
	exec.OrderLogger = func(o *store.TraderOrder, dec *CopyDecision, execErr error) {
		if orderLogger == nil || o == nil {
			return
		}
		o.TraderID = traderID
		orderLogger(o, dec, execErr)
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
	// 读取持久化游标
	if cfg.ProviderParams != nil {
		if lastSeqStr, ok := cfg.ProviderParams["last_seq"]; ok {
			if seq, err := strconv.ParseInt(lastSeqStr, 10, 64); err == nil {
				provider.SetCursor(seq)
			}
		}
	}
	// 从 provider_params 读取 baseline_snapshot 作为基线
	if cfg.ProviderParams != nil {
		if snapStr, ok := cfg.ProviderParams["baseline_snapshot"]; ok && snapStr != "" {
			var snap LeaderState
			if err := json.Unmarshal([]byte(snapStr), &snap); err == nil {
				service.SetBaseline(&snap)
			}
		}
	}
	return service, nil
}

// Snapshot 获取当前领航员快照（用于持久化基线）
func (s *Service) Snapshot() (*LeaderState, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("provider nil")
	}
	return s.provider.Snapshot(s.ctx)
}
