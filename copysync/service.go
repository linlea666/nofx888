package copysync

import (
	"context"
	"fmt"
	"math"
	"nofx/logger"
	"nofx/store"
	"strings"
	"sync"
	"time"
)

const positionEpsilon = 1e-9

// TrackedPositionStore 持久化跟踪状态以支持重启恢复。
type TrackedPositionStore interface {
	List(traderID string) ([]store.CopyTrackedPosition, error)
	Upsert(traderID, symbol, side string, followerSize float64) error
	Delete(traderID, symbol, side string) error
}

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
	ProviderEvent    ProviderEvent `json:"provider_event"`
	Symbol           string        `json:"symbol"`
	FollowerEquity   float64       `json:"follower_equity"`
	FollowerNotional float64       `json:"follower_notional"`
	FollowerQty      float64       `json:"follower_qty"`
	Price            float64       `json:"price"`
	PriceSource      string        `json:"price_source"`
	MinNotionalHit   bool          `json:"min_notional_hit"`
	MaxNotionalHit   bool          `json:"max_notional_hit"`
	Skipped          bool          `json:"skipped"`
	SkipReason       string        `json:"skip_reason"`
	CopySkipReason   string        `json:"copy_skip_reason"` // 文案
	ErrCode          string        `json:"err_code"`         // 分类错误码
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

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	trackedPositions map[string]float64
	trackerStore     TrackedPositionStore
	traderID         string
	strategy         CopyStrategy
}

// NewService 创建 CopySync 服务。
func NewService(cfg CopyConfig, provider Provider, account FollowerAccount, executor ExecutionAdapter, priceFallback func(symbol string) (float64, string, error), tracker TrackedPositionStore, traderID string, strategy CopyStrategy) *Service {
	cfg.EnsureDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	if strategy == nil {
		strategy = DualSidedStrategy{}
	}
	return &Service{
		cfg:              cfg,
		provider:         provider,
		account:          account,
		executor:         executor,
		priceFunc:        priceFallback,
		ctx:              ctx,
		cancel:           cancel,
		trackedPositions: make(map[string]float64),
		trackerStore:     tracker,
		traderID:         traderID,
		strategy:         strategy,
	}
}

func (s *Service) logSkip(ev ProviderEvent, reason string) {
	if s.loggerCb == nil {
		return
	}
	dec := &CopyDecision{
		ProviderEvent:  ev,
		Symbol:         NormalizeSymbol(ev.Symbol),
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

	s.restoreTrackedPositions()
	if err := s.provider.Start(s.ctx); err != nil {
		return fmt.Errorf("start provider: %w", err)
	}
	s.wg.Add(1)
	go s.loop()
	logger.Infof("📡 CopySync started for provider=%s", s.provider.Name())
	return nil
}

func (s *Service) restoreTrackedPositions() {
	if s.trackerStore == nil || s.traderID == "" {
		return
	}
	records, err := s.trackerStore.List(s.traderID)
	if err != nil {
		logger.Warnf("copysync: restore tracked positions failed: %v", err)
		return
	}
	for _, rec := range records {
		if rec.Symbol == "" || rec.Side == "" {
			continue
		}
		s.setTrackedInternal(rec.Symbol, rec.Side, rec.FollowerSize, false)
	}
	// 清理已经不存在的仓位，保持状态准确
	for key := range s.trackedPositions {
		sym, side := splitSymbolSide(key)
		if sym == "" || side == "" {
			delete(s.trackedPositions, key)
			continue
		}
		if size, err := s.currentFollowerPositionSize(sym, side); err == nil {
			if size <= positionEpsilon {
				s.clearTracked(sym, side)
			} else {
				s.setTrackedInternal(sym, side, size, false)
			}
		}
	}
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

	// 事件时效校验：超出窗口则丢弃，避免重放
	if s.cfg.StaleEventWindowSec > 0 && !ev.Timestamp.IsZero() {
		if time.Since(ev.Timestamp) > time.Duration(s.cfg.StaleEventWindowSec)*time.Second {
			logger.Infof("copysync: skip %s %s stale_event window=%ds evTime=%s", ev.Symbol, ev.Action, s.cfg.StaleEventWindowSec, ev.Timestamp.Format(time.RFC3339))
			s.logSkip(ev, "stale_event")
			return
		}
	}
	if s.strategy != nil {
		if skip, reason := s.strategy.BeforeEvent(s, ev); skip {
			if reason != "" {
				s.logSkip(ev, reason)
			}
			return
		}
	}
	// 如果是 reduce/close 但跟随端无仓位，则跳过，避免 reduce-only 报错
	if ev.Action == "reduce" || ev.Action == "close" {
		if !s.followerHasPosition(ev.Symbol, ev.Side) {
			logger.Infof("copysync: skip %s %s follower has no position", ev.Symbol, ev.Action)
			s.logSkip(ev, "follower_position_missing")
			return
		}
	}
	// 加仓必须建立在已有同向仓位的前提下，避免“历史老仓加仓”把本地误当新开
	if ev.Action == "add" {
		if !s.followerHasPosition(ev.Symbol, ev.Side) {
			logger.Infof("copysync: skip %s add follower has no position", ev.Symbol)
			// 使用 history_add_no_position 明确区分，前端可针对此做“保护跳过”展示
			s.logSkip(ev, "history_add_no_position")
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

	formulaParts := []string{
		fmt.Sprintf("按比例换算：领航员成交额 %.4f / 净值 %.4f = 原始比例 %.6f", leaderNotional, ev.LeaderEquity, rawRatio),
		fmt.Sprintf("跟随净值 %.4f × 跟单系数 %.2f%% => 目标成交额 %.4f", followerEquity, s.cfg.CopyRatio, followerNotional),
		fmt.Sprintf("价源 %s=%.4f => 下单数量 %.8f", priceSource, price, qty),
	}
	if minHit || maxHit {
		hits := []string{}
		if minHit {
			hits = append(hits, "命中最小成交额")
		}
		if maxHit {
			hits = append(hits, "命中最大成交额")
		}
		formulaParts = append(formulaParts, fmt.Sprintf("阈值：%s", strings.Join(hits, "，")))
	}

	decision := &CopyDecision{
		ProviderEvent:    ev,
		Symbol:           NormalizeSymbol(ev.Symbol),
		FollowerEquity:   followerEquity,
		FollowerNotional: followerNotional,
		FollowerQty:      qty,
		Price:            price,
		PriceSource:      priceSource,
		MinNotionalHit:   minHit,
		MaxNotionalHit:   maxHit,
		Formula:          strings.Join(formulaParts, " | "),
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
	logger.Infof("copysync: %s %s 跟单完成 | %s | provider=%s trace=%s", ev.Symbol, ev.Action, decision.Formula, ev.ProviderType, ev.TraceID)
	if err := s.afterExecution(ev, decision); err != nil {
		logger.Infof("copysync: post execution hook %s %s err=%v", ev.Symbol, ev.Action, err)
	}
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

// followerHasPosition 简单查询跟随端是否已有同向仓位（用于防重复开仓）。
func (s *Service) currentFollowerPositionSize(symbol, side string) (float64, error) {
	te, ok := s.executor.(*TraderExecutor)
	if !ok || te == nil || te.Trader == nil {
		return 0, fmt.Errorf("no trader executor")
	}
	positions, err := te.Trader.GetPositions()
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, p := range positions {
		ps, size, isLong, err := parsePosition(p)
		if err != nil {
			logger.Infof("copysync: ignore position parse error for follower position %v", err)
			continue
		}
		if SymbolsMatch(ps, symbol) && size > 0 {
			if (side == "long" && isLong) || (side == "short" && !isLong) {
				total += size
			}
		}
	}
	return total, nil
}

func (s *Service) followerHasPosition(symbol, side string) bool {
	size, err := s.currentFollowerPositionSize(symbol, side)
	if err != nil {
		logger.Infof("copysync: follower position query failed for %s %s: %v", symbol, side, err)
		return false
	}
	return size > positionEpsilon
}

func (s *Service) clearTracked(symbol, side string) {
	symbol = NormalizeSymbol(symbol)
	key := symbolSideKey(symbol, side)
	delete(s.trackedPositions, key)

	if s.trackerStore != nil && s.traderID != "" {
		if err := s.trackerStore.Delete(s.traderID, symbol, side); err != nil {
			logger.Warnf("copysync: clearTracked persist failed %s %s: %v", symbol, side, err)
		}
	}
}

func (s *Service) afterExecution(ev ProviderEvent, dec *CopyDecision) error {
	symbol := NormalizeSymbol(ev.Symbol)
	size, err := s.currentFollowerPositionSize(symbol, ev.Side)
	if err != nil {
		return err
	}
	if size <= positionEpsilon {
		s.clearTracked(symbol, ev.Side)
		return nil
	}
	s.setTrackedInternal(symbol, ev.Side, size, true)
	_ = dec
	return nil
}

func (s *Service) trackedSize(symbol, side string) float64 {
	if symbol == "" || side == "" {
		return 0
	}
	return s.trackedPositions[symbolSideKey(symbol, side)]
}

func (s *Service) setTracked(symbol, side string, size float64) {
	s.setTrackedInternal(symbol, side, size, true)
}

func (s *Service) setTrackedInternal(symbol, side string, size float64, persist bool) {
	symbol = NormalizeSymbol(symbol)
	key := symbolSideKey(symbol, side)
	s.trackedPositions[key] = size

	if persist && s.trackerStore != nil && s.traderID != "" {
		if err := s.trackerStore.Upsert(s.traderID, symbol, side, size); err != nil {
			logger.Warnf("copysync: setTracked persist failed %s %s: %v", symbol, side, err)
		}
	}
}

func (s *Service) refreshTrackedState(ev ProviderEvent, dec *CopyDecision) {
	symbol := NormalizeSymbol(ev.Symbol)
	size, err := s.currentFollowerPositionSize(symbol, ev.Side)
	if err != nil {
		logger.Warnf("copysync: refreshTrackedState query failed %s %s: %v", symbol, ev.Side, err)
		return
	}
	if size <= positionEpsilon {
		s.clearTracked(symbol, ev.Side)
		return
	}
	_ = dec
	s.setTrackedInternal(symbol, ev.Side, size, false)
}

func splitSymbolSide(key string) (string, string) {
	idx := strings.LastIndex(key, "_")
	if idx <= 0 || idx >= len(key)-1 {
		return key, ""
	}
	return key[:idx], key[idx+1:]
}

func symbolSideKey(symbol, side string) string {
	return fmt.Sprintf("%s_%s", NormalizeSymbol(symbol), side)
}
