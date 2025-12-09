package copysync

import (
	"context"
	"fmt"
	"math"
	"nofx/logger"
	"strconv"
	"strings"
	"sync"
	"time"
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
	ProviderEvent   ProviderEvent `json:"provider_event"`
	FollowerEquity  float64       `json:"follower_equity"`
	FollowerNotional float64      `json:"follower_notional"`
	FollowerQty     float64       `json:"follower_qty"`
	Price           float64       `json:"price"`
	PriceSource     string        `json:"price_source"`
	MinNotionalHit  bool          `json:"min_notional_hit"`
	MaxNotionalHit  bool          `json:"max_notional_hit"`
	Skipped         bool          `json:"skipped"`
	SkipReason      string        `json:"skip_reason"`
	CopySkipReason  string        `json:"copy_skip_reason"` // 文案
	ErrCode         string        `json:"err_code"`         // 分类错误码
	// 公式展示辅助
	Formula string `json:"formula"`
}

// Service 读取 ProviderEvent，做比例换算与基础风控，再交给 ExecutionAdapter。
type Service struct {
	cfg       CopyConfig
	provider  Provider
	account   FollowerAccount
	executor  ExecutionAdapter
	priceFunc func(symbol string) (float64, string, error) // 行情兜底，返回价格及价源
	loggerCb  func(decision *CopyDecision)

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	baseline *LeaderState
}

// NewService 创建 CopySync 服务。
func NewService(cfg CopyConfig, provider Provider, account FollowerAccount, executor ExecutionAdapter, priceFallback func(symbol string) (float64, string, error)) *Service {
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
		ProviderEvent:  ev,
		Skipped:        true,
		SkipReason:     reason,
		CopySkipReason: reason,
		ErrCode:        ClassifyErr(reason),
	}
	s.loggerCb(dec)
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
	// 基线快照（用于过滤已有仓位）仅在可用时加载一次，失败则重试几次
	if snap, err := s.provider.Snapshot(s.ctx); err == nil {
		if s.baseline == nil {
			s.baseline = snap
		} else {
			// 更新基线时间戳，避免使用过旧数据
			s.baseline.Timestamp = snap.Timestamp
		}
	} else {
		logger.Warnf("copysync: snapshot failed on start: %v, will retry", err)
		go s.retrySnapshot()
	}

	if err := s.provider.Start(s.ctx); err != nil {
		return fmt.Errorf("start provider: %w", err)
	}
	// 周期性刷新基线，避免长期运行后快照失效
	go s.refreshBaselineLoop()
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
		// 快照过期则忽略基线
		if !s.baseline.Timestamp.IsZero() && time.Since(s.baseline.Timestamp) > 2*time.Hour {
			logger.Infof("copysync: baseline expired, refreshing before %s", ev.Symbol)
			if snap, err := s.provider.Snapshot(s.ctx); err == nil {
				s.SetBaseline(snap)
				s.reconcileFollowerPositions()
			} else {
				// 基线失效，避免一直阻塞，清空
				s.baseline = nil
			}
		} else {
			keyLong := fmt.Sprintf("%s_long", ev.Symbol)
			keyShort := fmt.Sprintf("%s_short", ev.Symbol)
			pos := s.baseline.Positions[keyLong]
			if pos == nil {
				pos = s.baseline.Positions[keyShort]
			}
			if pos != nil && pos.Size > 0 {
				// 如果是 close 动作允许通过，否则跳过
				if ev.Action != "close" && ev.Action != "reduce" {
					logger.Infof("copysync: skip %s %s due to baseline position", ev.Symbol, ev.Action)
					s.logSkip(ev, "baseline_skip")
					return
				}
			}
		}
	}
	// 额外防重复：若跟随端已有同向仓位且事件为开/加仓，跳过；若存在反向仓位则先尝试平掉
	if ev.Action == "open" || ev.Action == "add" {
		if s.handleFollowerPositions(ev) {
			return
		}
	}

	price := ev.Price
	priceSource := ev.PriceSource
	if price <= 0 && s.cfg.PriceFallbackEnabled && s.priceFunc != nil {
		backoffs := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
		attempts := 0
		for i, d := range backoffs {
			attempts++
			if p, src, err := s.priceFunc(ev.Symbol); err == nil && p > 0 {
				price = p
				if src != "" {
					priceSource = src
				} else {
					priceSource = "market"
				}
				break
			}
			time.Sleep(d)
			if i == len(backoffs)-1 && price <= 0 {
				reason := fmt.Sprintf("price_source_down attempts=%d", attempts)
				ev.ErrCode = "price_source_down"
				s.logSkip(ev, reason)
				return
			}
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
		s.logSkip(ev, "follower_equity_missing")
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
		ProviderEvent:   ev,
		FollowerEquity:  followerEquity,
		FollowerNotional: followerNotional,
		FollowerQty:     qty,
		Price:           price,
		PriceSource:     priceSource,
		MinNotionalHit:  minHit,
		MaxNotionalHit:  maxHit,
		Formula:         fmt.Sprintf("follow_notional=max(min, (%.4f/%.4f)*%.4f*%.2f%%)=%.4f qty=%.8f", leaderNotional, ev.LeaderEquity, followerEquity, s.cfg.CopyRatio, followerNotional, qty),
	}

	if err := s.executor.ExecuteCopy(s.ctx, decision); err != nil {
		decision.Skipped = true
		decision.SkipReason = err.Error()
		decision.ErrCode = ClassifyErr(decision.SkipReason)
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

func (s *Service) retrySnapshot() {
	for attempt := 1; ; attempt++ {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(10 * time.Second):
			snap, err := s.provider.Snapshot(s.ctx)
			if err != nil {
				logger.Warnf("copysync: retry snapshot failed (%d): %v", attempt, err)
				continue
			}
			logger.Infof("copysync: snapshot retry success after %d attempt(s)", attempt)
			s.SetBaseline(snap)
			s.reconcileFollowerPositions()
			return
		}
	}
}

// refreshBaselineLoop 周期性刷新基线（每30分钟）。
func (s *Service) refreshBaselineLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			snap, err := s.provider.Snapshot(s.ctx)
			if err != nil {
				logger.Warnf("copysync: refresh baseline failed: %v", err)
				continue
			}
			s.SetBaseline(snap)
			logger.Infof("copysync: baseline refreshed at %s", time.Now().Format(time.RFC3339))
			s.reconcileFollowerPositions()
		}
	}
}

// followerHasPosition 简单查询跟随端是否已有同向仓位（用于防重复开仓）。
func (s *Service) followerHasPosition(symbol, side string) bool {
	te, ok := s.executor.(*TraderExecutor)
	if !ok || te == nil || te.Trader == nil {
		return false
	}
	positions, err := te.Trader.GetPositions()
	if err != nil {
		return false
	}
	for _, p := range positions {
		ps, size, isLong := parsePosition(p)
		if ps == symbol && size > 0 {
			if (side == "long" && isLong) || (side == "short" && !isLong) {
				return true
			}
		}
	}
	return false
}

// reconcileFollowerPositions 对比基线和跟随端持仓，必要时强制平掉残留/反向仓。
func (s *Service) reconcileFollowerPositions() {
	te, ok := s.executor.(*TraderExecutor)
	if !ok || te == nil || te.Trader == nil {
		return
	}
	if s.baseline == nil || s.baseline.Positions == nil {
		return
	}
	positions, err := te.Trader.GetPositions()
	if err != nil {
		return
	}
	for _, p := range positions {
		sym, size, isLong := parsePosition(p)
		if sym == "" || size <= 0 {
			continue
		}
		side := "long"
		if !isLong {
			side = "short"
		}
		key := fmt.Sprintf("%s_%s", sym, side)
		basePos := s.baseline.Positions[key]
		if basePos == nil || basePos.Size <= 0 {
			logger.Warnf("copysync: reconcile close residual position %s %s size=%.4f", sym, side, size)
			_ = te.close(nil, side, sym, size)
			continue
		}
		// 方向一致但数量超出基线，平掉差额
		if size > basePos.Size {
			diff := size - basePos.Size
			logger.Warnf("copysync: reconcile trim position %s %s diff=%.4f", sym, side, diff)
			_ = te.close(nil, side, sym, diff)
		}
		// 方向相反（基线方向与当前不符），平掉当前全部
		if basePos.Side != "" && basePos.Side != side {
			logger.Warnf("copysync: reconcile opposite position %s follower=%s base=%s size=%.4f", sym, side, basePos.Side, size)
			_ = te.close(nil, side, sym, size)
		}
	}
}

// handleFollowerPositions 在开/加仓前检查跟随端持仓，处理同向/反向残留。
// 返回 true 表示已处理并需跳过本次事件。
func (s *Service) handleFollowerPositions(ev ProviderEvent) bool {
	te, ok := s.executor.(*TraderExecutor)
	if !ok || te == nil || te.Trader == nil {
		return false
	}
	positions, err := te.Trader.GetPositions()
	if err != nil {
		return false
	}
	opposites := []struct {
		side string
		size float64
	}{}
	hasSame := false
	for _, p := range positions {
		ps, size, isLong := parsePosition(p)
		if ps != ev.Symbol || size <= 0 {
			continue
		}
		if (ev.Side == "long" && isLong) || (ev.Side == "short" && !isLong) {
			hasSame = true
		} else {
			side := "short"
			if isLong {
				side = "long"
			}
			opposites = append(opposites, struct {
				side string
				size float64
			}{side: side, size: size})
		}
	}

	// 先处理反向仓位：尝试强制平掉
	for _, o := range opposites {
		if err := te.close(nil, o.side, ev.Symbol, o.size); err != nil {
			logger.Infof("copysync: skip %s %s due to opposite position close failed: %v", ev.Symbol, ev.Action, err)
			s.logSkip(ev, "insufficient_position")
			return true
		}
	}

	// 同向仓位存在则跳过开/加仓
	if hasSame {
		logger.Infof("copysync: skip %s %s due to follower position exists", ev.Symbol, ev.Action)
		s.logSkip(ev, "follower_position_exists")
		return true
	}
	return false
}
