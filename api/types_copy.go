package api

// CopyTradingConfig 用于 API 层收发跟单配置。
type CopyTradingConfig struct {
	ProviderType         string            `json:"provider_type"`
	ProviderParams       map[string]string `json:"provider_params,omitempty"`
	CopyRatio            float64           `json:"copy_ratio"`
	MinNotional          float64           `json:"min_notional"`
	MaxNotional          float64           `json:"max_notional"`
	FollowOpen           bool              `json:"follow_open"`
	FollowAdd            bool              `json:"follow_add"`
	FollowReduce         bool              `json:"follow_reduce"`
	FollowClose          bool              `json:"follow_close"`
	LeverageSync         bool              `json:"leverage_sync"`
	MarginModeSync       bool              `json:"margin_mode_sync"`
	PriceFallbackEnabled bool              `json:"price_fallback_enabled"`
}

