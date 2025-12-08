package copysync

import (
	"context"
	"time"
)

// Provider 抽象任何领航员信号源（AI、钱包等）
// 应当在 Start 后不断向 Events 通道推送 ProviderEvent。
type Provider interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Events() <-chan ProviderEvent
	// Snapshot 可用于在重启或对账时获取领航员当前持仓/净值状态。
	Snapshot(ctx context.Context) (*LeaderState, error)
	// Cursor 用于持久化游标（例如 last seq/time）
	GetCursor() int64
	SetCursor(v int64)
}

// ProviderEvent 领航员单条交易事件（成交/仓位变动）。
type ProviderEvent struct {
	TraceID      string    `json:"trace_id"`
	SourceID     string    `json:"source_id"`
	Symbol       string    `json:"symbol"`
	Side         string    `json:"side"`         // buy/sell 或 long/short
	Action       string    `json:"action"`       // open/add/reduce/close
	Price        float64   `json:"price"`        // 事件价格（成交价）；缺价时由价源兜底
	PriceSource  string    `json:"price_source"` // fill / market
	Size         float64   `json:"size"`         // 合约张数或币数量（与 exchange 具体单位一致）
	Notional     float64   `json:"notional"`     // 成交额（quote 计价），优先使用
	Leverage     float64   `json:"leverage"`     // 领航员杠杆
	MarginMode   string    `json:"margin_mode"`  // cross / isolated
	MarginUsed   float64   `json:"margin_used"`  // 该笔或该仓保证金（若可得）
	LeaderEquity float64   `json:"leader_equity"`// 领航员净值，用于比例换算
	ProviderType string    `json:"provider_type"`// provider 类型，便于日志
	ErrCode      string    `json:"err_code"`     // 额外错误（如缺价）
	Timestamp    time.Time `json:"timestamp"`
	Seq          int64     `json:"seq"` // 序号/游标，便于幂等和断点续传
}

// LeaderState 描述领航员当前账户与持仓的概要信息。
type LeaderState struct {
	Equity    float64                      `json:"equity"`
	Positions map[string]*LeaderPosition   `json:"positions"` // key: symbol_side
	Timestamp time.Time                    `json:"timestamp"`
	Extra     map[string]map[string]string `json:"extra,omitempty"` // 预留原始字段
}

// LeaderPosition 领航员单个持仓信息。
type LeaderPosition struct {
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`        // long/short
	Size       float64 `json:"size"`        // 仓位数量
	EntryPrice float64 `json:"entry_price"` // 开仓均价
	MarginUsed float64 `json:"margin_used"` // 保证金
	Leverage   float64 `json:"leverage"`    // 杠杆
	MarginMode string  `json:"margin_mode"` // cross / isolated
}
