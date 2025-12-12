package copysync

import (
	"fmt"
	"nofx/market"
	"nofx/store"
	"strconv"
)

// NewServiceForTrader 根据 CopyConfig 与 provider 选择创建 CopySync Service。
// followerTrader：跟随账户的交易适配器；用于取净值与下单。
func NewServiceForTrader(cfg CopyConfig, followerTrader TraderAdapter, traderID string, orderLogger func(o *store.TraderOrder, dec *CopyDecision, execErr error), st *store.Store) (*Service, error) {
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
		provider = NewOKXPositionProvider(uniqueName)
	case "hl_wallet", "hyperliquid_wallet":
		addr := ""
		if cfg.ProviderParams != nil {
			addr = cfg.ProviderParams["address"]
		}
		if addr == "" {
			return nil, fmt.Errorf("copysync: hyperliquid_wallet requires provider_params.address")
		}
		// 默认使用 WS 模式；若需要 HTTP 兜底可改为 API Provider
		provider = NewHyperliquidWSProvider(addr)
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

	// 行情兜底价：优先使用跟随交易所返回的价格，其次使用通用行情（last/mid/mark）。
	priceFunc := func(symbol string) (float64, string, error) {
		// 1) 跟随端行情（与下单所用交易所一致）
		if followerTrader != nil {
			if p, err := followerTrader.GetMarketPrice(symbol); err == nil && p > 0 {
				return p, "follower_ticker", nil
			}
		}

		// 2) 聚合行情：先 last，再 mid
		data, err := market.Get(symbol)
		if err == nil && data != nil {
			if data.CurrentPrice > 0 {
				return data.CurrentPrice, "last", nil
			}
			// mid 价：取主时间框 mid 末值
			if data.TimeframeData != nil {
				for _, tf := range data.TimeframeData {
					if tf != nil && len(tf.MidPrices) > 0 {
						last := tf.MidPrices[len(tf.MidPrices)-1]
						if last > 0 {
							return last, "mid", nil
						}
					}
				}
			}
		}
		// 3) mark 价兜底
		if mp, err := market.GetMarkPrice(symbol); err == nil && mp > 0 {
			return mp, "mark", nil
		}
		return 0, "", fmt.Errorf("price_fallback_failed")
	}

	var tracker TrackedPositionStore
	if st != nil {
		tracker = st.CopyTracker()
	}
	service := NewService(cfg, provider, account, exec, priceFunc, tracker, traderID)
	// 读取持久化游标
	if cfg.ProviderParams != nil {
		if lastSeqStr, ok := cfg.ProviderParams["last_seq"]; ok {
			if seq, err := strconv.ParseInt(lastSeqStr, 10, 64); err == nil {
				provider.SetCursor(seq)
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
