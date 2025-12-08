package copysync

import (
	"context"
	"fmt"
	"math"
	"nofx/logger"
	"sync"
	"strings"
)

// FollowerAccount 获取跟随账户净值。
type FollowerAccount interface {
	GetEquity(ctx context.Context) (float64, error)
}

// ExecutionAdapter 执行具体下单逻辑，由交易所适配器实现。
type ExecutionAdapter interface {
	ExecuteCopy(ctx context.Context, decision *CopyDecision) error
}

// CopyDecision 已计算好的跟单指令，传递给 ExecutionAdapter。
type CopyDecision struct {
	ProviderEvent ProviderEvent `json:"provider_event"`
	FollowerEquity float64      `json:"follower_equity"`
	FollowerNotional float64    `json:"follower_notional"`
	FollowerQty     float64     `json:"follower_qty"`
	Price           float64     `json:"price"`
	PriceSource     string      `json:"price_source"`
	MinNotionalHit  bool        `json:"min_notional_hit"`
	MaxNotionalHit  bool        `json:"max_notional_hit"`
	Skipped         bool        `json:"skipped"`
	SkipReason      string      `json:"skip_reason"`
	CopySkipReason  string      `json:"copy_skip_reason"`
	// 公式展示辅助
	Formula string `json:"formula"`
}

// Service 读取 ProviderEvent，做比例换算与基础风控，再交给 ExecutionAdapter。
type Service struct {
	cfg       CopyConfig
	provider  Provider
	account   FollowerAccount
	executor  ExecutionAdapter
	priceFunc func(symbol string) (float64, error) // 行情兜底
	loggerCb  func(decision *CopyDecision)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	baseline *LeaderState
}

// NewService 创建 CopySync 服务。
func NewService(cfg CopyConfig, provider Provider, account FollowerAccount, executor ExecutionAdapter, priceFallback func(symbol string) (float64, error)) *Service {
	cfg.EnsureDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		cfg:       cfg,
		provider:  provider,
		account:   account,
		executor:  executor,
		priceFunc: priceFallback,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// SetBaseline 设置领航员基线快照（已有仓位不跟）。
func (s *Service) SetBaseline(state *LeaderState) {
	s.baseline = state
}

func (s *Service) logSkip(ev ProviderEvent, reason string) {
	if s.loggerCb == nil {
		return
	}
	dec := &CopyDecision{
		ProviderEvent: ev,
		Skipped:       true,
		SkipReason:    reason,
		CopySkipReason: reason,
	}
	s.loggerCb(dec)
}

// classifyErr 简单错误码映射。
func classifyErr(msg string) string {
	if msg == "" {
		return ""
	}
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "min qty"):
		return "min_qty_not_met"
	case strings.Contains(l, "min notional"):
		return "min_notional_not_met"
	case strings.Contains(l, "insufficient"):
		return "insufficient_balance"
	case strings.Contains(l, "price"):
		return "price_missing"
	default:
		return "exchange_reject"
	}
}

// ClassifyErr 导出错误码分类，便于外部复用（日志/前端展示一致）。
func ClassifyErr(msg string) string {
	return classifyErr(msg)
}

// WithLogger 设置决策日志回调。
func (s *Service) WithLogger(cb func(decision *CopyDecision)) {
	s.loggerCb = cb
}

// Start 启动 provider 并开始消费事件。
func (s *Service) Start() error {
	if s.provider == nil || s.account == nil || s.executor == nil {
		return fmt.Errorf("copysync: missing provider/account/executor")
	}
	// 基线快照（用于过滤已有仓位）仅在可用时加载一次
	if snap, err := s.provider.Snapshot(s.ctx); err == nil {
		if s.baseline == nil {
			s.baseline = snap
		} else {
			// 更新基线时间戳，避免使用过旧数据
			s.baseline.Timestamp = snap.Timestamp
		}
	}
	if err := s.provider.Start(s.ctx); err != nil {
		return fmt.Errorf("start provider: %w", err)
	}
	s.wg.Add(1)
	go s.loop()
	logger.Infof("📡 CopySync started for provider=%s", s.provider.Name())
	return nil
}

// Stop 停止服务。
func (s *Service) Stop() {
	s.cancel()
	_ = s.provider.Stop(context.Background())
	s.wg.Wait()
	logger.Info("📡 CopySync stopped")
}

// ProviderCursor 返回 provider 当前游标（用于持久化）。
func (s *Service) ProviderCursor() int64 {
	if s.provider == nil {
		return 0
	}
	return s.provider.GetCursor()
}

func (s *Service) loop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case ev, ok := <-s.provider.Events():
			if !ok {
				logger.Info("copysync: provider event channel closed")
				return
			}
			s.handleEvent(ev)
		}
	}
}

// handleEvent 做比例换算与基础风控。
func (s *Service) handleEvent(ev ProviderEvent) {
	// 基本开关检查
	if !s.shouldFollow(ev.Action) {
		logger.Infof("copysync: skip %s %s due to follow switch off", ev.Symbol, ev.Action)
		return
	}

	// 基线过滤：已有仓位不跟（直到归零后重新开仓）
	if s.baseline != nil && s.baseline.Positions != nil {
		keyLong := fmt.Sprintf("%s_long", ev.Symbol)
		keyShort := fmt.Sprintf("%s_short", ev.Symbol)
		if s.baseline.Positions[keyLong] != nil || s.baseline.Positions[keyShort] != nil {
			// 如果是 close 动作允许通过，否则跳过
			if ev.Action != "close" && ev.Action != "reduce" {
				logger.Infof("copysync: skip %s %s due to baseline position", ev.Symbol, ev.Action)
				s.logSkip(ev, "baseline_skip")
				return
			}
		}
	}

	price := ev.Price
	priceSource := ev.PriceSource
	if price <= 0 && s.cfg.PriceFallbackEnabled && s.priceFunc != nil {
		if p, err := s.priceFunc(ev.Symbol); err == nil && p > 0 {
			price = p
			priceSource = "market"
		} else if err != nil {
			ev.ErrCode = "price_fallback_failed"
			s.logSkip(ev, ev.ErrCode)
			return
		}
	}
	if price <= 0 {
		logger.Infof("copysync: skip %s %s no price available", ev.Symbol, ev.Action)
		s.logSkip(ev, "price_missing")
		return
	}

	leaderNotional := ev.Notional
	if leaderNotional <= 0 && ev.Size > 0 {
		leaderNotional = price * ev.Size
	}
	if leaderNotional <= 0 || ev.LeaderEquity <= 0 {
		logger.Infof("copysync: skip %s %s no leader notional/equity", ev.Symbol, ev.Action)
		s.logSkip(ev, "leader_notional_missing")
		return
	}

	followerEquity, err := s.account.GetEquity(s.ctx)
	if err != nil || followerEquity <= 0 {
		logger.Infof("copysync: skip %s %s cannot get follower equity: %v", ev.Symbol, ev.Action, err)
		return
	}

	rawRatio := (leaderNotional / ev.LeaderEquity)
	followerNotional := rawRatio * followerEquity * (s.cfg.CopyRatio / 100.0)
	minHit := false
	maxHit := false
	if followerNotional < s.cfg.MinNotional {
		followerNotional = s.cfg.MinNotional
		minHit = true
	}
	if s.cfg.MaxNotional > 0 && followerNotional > s.cfg.MaxNotional {
		followerNotional = s.cfg.MaxNotional
		maxHit = true
	}

	qty := followerNotional / price
	if qty <= 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
		logger.Infof("copysync: skip %s %s invalid qty computed", ev.Symbol, ev.Action)
		s.logSkip(ev, "qty_invalid")
		return
	}

	decision := &CopyDecision{
		ProviderEvent:  ev,
		FollowerEquity: followerEquity,
		FollowerNotional: followerNotional,
		FollowerQty:    qty,
		Price:          price,
		PriceSource:    priceSource,
		MinNotionalHit: minHit,
		MaxNotionalHit: maxHit,
		Formula:        fmt.Sprintf("follow_notional=max(min, (%.4f/%.4f)*%.4f*%.2f%%)=%.4f qty=%.8f", leaderNotional, ev.LeaderEquity, followerEquity, s.cfg.CopyRatio, followerNotional, qty),
	}

	if err := s.executor.ExecuteCopy(s.ctx, decision); err != nil {
		decision.Skipped = true
		decision.SkipReason = err.Error()
		decision.CopySkipReason = classifyErr(decision.SkipReason)
		logger.Infof("copysync: execute %s %s failed: %v (trace=%s)", ev.Symbol, ev.Action, err, ev.TraceID)
		if s.loggerCb != nil {
			s.loggerCb(decision)
		}
		return
	}
	logger.Infof("copysync: execute %s %s ok qty=%.8f notional=%.4f price=%.4f source=%s minHit=%v maxHit=%v provider=%s trace=%s",
		ev.Symbol, ev.Action, qty, followerNotional, price, priceSource, minHit, maxHit, ev.ProviderType, ev.TraceID)
	if s.loggerCb != nil {
		s.loggerCb(decision)
	}
}

func (s *Service) shouldFollow(action string) bool {
	switch action {
	case "open":
		return s.cfg.FollowOpen
	case "add":
		return s.cfg.FollowAdd
	case "reduce":
		return s.cfg.FollowReduce
	case "close":
		return s.cfg.FollowClose
	default:
		return false
	}
}
