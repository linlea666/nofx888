package copysync

// CopyConfig 跟单比例与阈值配置。
type CopyConfig struct {
	ProviderType          string  `json:"provider_type"`                      // ai / okx_wallet / hl_wallet / ...
	ProviderParams        map[string]string `json:"provider_params,omitempty"` // 比如 uniqueName/address
	CopyRatio             float64 `json:"copy_ratio"`                        // 跟单系数，100=1倍
	MinNotional           float64 `json:"min_notional"`                      // 最小成交额（quote）
	MaxNotional           float64 `json:"max_notional"`                      // 最大成交额（0 表示不限）
	FollowOpen            bool    `json:"follow_open"`                       // 是否跟随开仓
	FollowAdd             bool    `json:"follow_add"`                        // 是否跟随加仓
	FollowReduce          bool    `json:"follow_reduce"`                     // 是否跟随减仓
	FollowClose           bool    `json:"follow_close"`                      // 是否跟随平仓
	LeverageSync          bool    `json:"leverage_sync"`                     // 是否同步杠杆
	MarginModeSync        bool    `json:"margin_mode_sync"`                  // 是否同步保证金模式
	PriceFallbackEnabled  bool    `json:"price_fallback_enabled"`            // 缺价是否兜底行情
	BaselineTimestampUnix int64   `json:"baseline_timestamp_unix,omitempty"` // 基线时间，已有仓位不跟（仅记录）
}

// EnsureDefaults 填充默认值，避免空配置导致分母为 0。
func (c *CopyConfig) EnsureDefaults() {
	if c.CopyRatio == 0 {
		c.CopyRatio = 100
	}
	if c.FollowOpen || c.FollowAdd || c.FollowReduce || c.FollowClose {
		return
	}
	// 默认跟随开/加/减/平全开
	c.FollowOpen = true
	c.FollowAdd = true
	c.FollowReduce = true
	c.FollowClose = true
}
