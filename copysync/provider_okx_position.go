package copysync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

// OKXPositionProvider 通过持仓变化生成事件，成交记录仅作校验/补充。
type OKXPositionProvider struct {
	uniqueName  string
	events      chan ProviderEvent
	cursor      int64 // 使用持仓更新时间 uTime 作为游标
	tradeCursor int64 // 记录 trade-records 的 fillTime 游标，仅用于日志

	httpClient *http.Client
	cancel     context.CancelFunc
	wg         sync.WaitGroup

	snapshotMu sync.RWMutex
	positions  map[string]*okxPosition // key: instId|posSide
	equity     float64
}

type okxPosition struct {
	InstID   string
	PosSide  string
	Pos      float64
	AvgPx    float64
	MarkPx   float64
	Lever    float64
	MgnMode  string
	Margin   float64
	UTime    int64
	Notional float64
}

// NewOKXPositionProvider 创建 OKX 基于持仓的 Provider。
func NewOKXPositionProvider(uniqueName string) Provider {
	return &OKXPositionProvider{
		uniqueName: uniqueName,
		events:     make(chan ProviderEvent, 256),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		positions:  make(map[string]*okxPosition),
	}
}

func (p *OKXPositionProvider) Name() string { return "okx_position" }

func (p *OKXPositionProvider) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)

	// 初始快照
	if err := p.refreshPositions(ctx, true); err != nil {
		return err
	}
	_ = p.refreshEquity(ctx)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.refreshPositions(ctx, false); err != nil {
					continue
				}
			}
		}
	}()

	// 低频更新净值
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = p.refreshEquity(ctx)
			}
		}
	}()

	// 低频拉取 trade-records 作为补充日志/校验
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = p.refreshTrades(ctx)
			}
		}
	}()

	return nil
}

func (p *OKXPositionProvider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	return nil
}

func (p *OKXPositionProvider) Events() <-chan ProviderEvent { return p.events }

func (p *OKXPositionProvider) Snapshot(ctx context.Context) (*LeaderState, error) {
	if err := p.refreshPositions(ctx, true); err != nil {
		return nil, err
	}
	p.snapshotMu.RLock()
	defer p.snapshotMu.RUnlock()
	state := &LeaderState{
		Equity:    p.equity,
		Positions: make(map[string]*LeaderPosition),
		Timestamp: time.Now(),
	}
	for k, pos := range p.positions {
		state.Positions[k] = &LeaderPosition{
			Symbol:     p.convertInstId(pos.InstID),
			Side:       pos.PosSide,
			Size:       pos.Pos,
			EntryPrice: pos.AvgPx,
			MarginUsed: pos.Margin,
			Leverage:   pos.Lever,
			MarginMode: pos.MgnMode,
		}
	}
	return state, nil
}

func (p *OKXPositionProvider) GetCursor() int64  { return p.cursor }
func (p *OKXPositionProvider) SetCursor(v int64) { p.cursor = v }

func (p *OKXPositionProvider) refreshPositions(ctx context.Context, initial bool) error {
	url := fmt.Sprintf("https://www.okx.com/priapi/v5/ecotrade/public/community/user/position-current?uniqueName=%s", p.uniqueName)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var parsed struct {
		Code string `json:"code"`
		Data []struct {
			PosData []struct {
				InstID   string `json:"instId"`
				PosSide  string `json:"posSide"`
				Pos      string `json:"pos"`
				AvgPx    string `json:"avgPx"`
				MarkPx   string `json:"markPx"`
				Lever    string `json:"lever"`
				MgnMode  string `json:"mgnMode"`
				Margin   string `json:"margin"`
				UTime    string `json:"uTime"`
				Notional string `json:"notionalUsd"`
			} `json:"posData"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if parsed.Code != "0" {
		return fmt.Errorf("okx position code=%s", parsed.Code)
	}

	newPositions := make(map[string]*okxPosition)
	latestCursor := p.cursor
	for _, block := range parsed.Data {
		for _, pos := range block.PosData {
			posVal, _ := strconv.ParseFloat(pos.Pos, 64)
			avgPx, _ := strconv.ParseFloat(pos.AvgPx, 64)
			markPx, _ := strconv.ParseFloat(pos.MarkPx, 64)
			lever, _ := strconv.ParseFloat(pos.Lever, 64)
			margin, _ := strconv.ParseFloat(pos.Margin, 64)
			uTime, _ := strconv.ParseInt(pos.UTime, 10, 64)
			notional, _ := strconv.ParseFloat(pos.Notional, 64)
			key := pos.InstID + "|" + pos.PosSide
			newPositions[key] = &okxPosition{
				InstID:   pos.InstID,
				PosSide:  pos.PosSide,
				Pos:      posVal,
				AvgPx:    avgPx,
				MarkPx:   markPx,
				Lever:    lever,
				MgnMode:  pos.MgnMode,
				Margin:   margin,
				UTime:    uTime,
				Notional: notional,
			}
			if uTime > latestCursor {
				latestCursor = uTime
			}
		}
	}

	if initial {
		p.snapshotMu.Lock()
		p.positions = newPositions
		p.cursor = latestCursor
		p.snapshotMu.Unlock()
		return nil
	}

	// 比较新旧持仓，生成事件
	p.snapshotMu.RLock()
	oldPositions := p.positions
	p.snapshotMu.RUnlock()

	p.emitPositionDiffs(oldPositions, newPositions)

	p.snapshotMu.Lock()
	p.positions = newPositions
	p.cursor = latestCursor
	p.snapshotMu.Unlock()
	return nil
}

func (p *OKXPositionProvider) emitPositionDiffs(oldPos, newPos map[string]*okxPosition) {
	keys := make(map[string]bool)
	for k := range oldPos {
		keys[k] = true
	}
	for k := range newPos {
		keys[k] = true
	}

	equity := p.equity

	for k := range keys {
		o := oldPos[k]
		n := newPos[k]

		if o == nil && n == nil {
			continue
		}
		if o == nil && n != nil {
			// 开仓
			p.emitEvent(n, "open", n.Pos, equity)
			continue
		}
		if o != nil && n == nil {
			// 全部平仓
			p.emitEvent(o, "close", o.Pos, equity)
			continue
		}
		// 同方向变化
		if o.PosSide == n.PosSide {
			delta := n.Pos - o.Pos
			if delta > 0 {
				act := "add"
				if o.Pos == 0 {
					act = "open"
				}
				p.emitEvent(n, act, delta, equity)
			} else if delta < 0 {
				act := "reduce"
				if n.Pos == 0 {
					act = "close"
				}
				p.emitEvent(n, act, -delta, equity)
			}
		} else {
			// 方向切换：先平旧，再开新
			if o.Pos > 0 {
				p.emitEvent(o, "close", o.Pos, equity)
			}
			if n.Pos > 0 {
				p.emitEvent(n, "open", n.Pos, equity)
			}
		}
	}
}

func (p *OKXPositionProvider) emitEvent(pos *okxPosition, action string, qty float64, equity float64) {
	price := pos.AvgPx
	priceSource := "avg"
	if price <= 0 && pos.MarkPx > 0 {
		price = pos.MarkPx
		priceSource = "mark"
	}
	if price <= 0 {
		priceSource = ""
	}
	ev := ProviderEvent{
		TraceID:      fmt.Sprintf("okx-pos-%s-%d", pos.InstID, time.Now().UnixNano()),
		SourceID:     p.uniqueName,
		ProviderType: "okx_position",
		Symbol:       p.convertInstId(pos.InstID),
		Side:         pos.PosSide,
		Action:       action,
		Price:        price,
		PriceSource:  priceSource,
		Size:         qty,
		Notional:     price * qty,
		Leverage:     pos.Lever,
		MarginMode:   pos.MgnMode,
		MarginUsed:   pos.Margin,
		LeaderEquity: equity,
		Timestamp:    time.Now(),
		Seq:          pos.UTime,
	}
	select {
	case p.events <- ev:
	default:
	}
}

func (p *OKXPositionProvider) refreshEquity(ctx context.Context) error {
	url := fmt.Sprintf("https://www.okx.com/priapi/v5/ecotrade/public/community/user/asset?uniqueName=%s", p.uniqueName)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var parsed struct {
		Code string `json:"code"`
		Data []struct {
			Currency string `json:"currency"`
			Amount   string `json:"amount"`
			Percent  string `json:"percent"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if parsed.Code != "0" {
		return fmt.Errorf("okx asset code=%s", parsed.Code)
	}
	// 以 USDT 资产为净值（社区接口提供的可用）
	for _, v := range parsed.Data {
		if strings.EqualFold(v.Currency, "USDT") {
			if amt, err := strconv.ParseFloat(v.Amount, 64); err == nil {
				p.equity = amt
			}
			break
		}
	}
	return nil
}

func (p *OKXPositionProvider) convertInstId(instID string) string {
	// 直接返回 instID，或去掉 "-SWAP"
	return instID
}

// refreshTrades 拉取 OKX 社区成交记录，作为补充日志（不生成事件）。
func (p *OKXPositionProvider) refreshTrades(ctx context.Context) error {
	// 使用 tradeCursor 作为 startModify，默认近 24h 窗口
	now := time.Now().UnixMilli()
	start := p.tradeCursor
	if start == 0 {
		start = now - 24*60*60*1000
	}
	url := fmt.Sprintf("https://www.okx.com/priapi/v5/ecotrade/public/community/user/trade-records?uniqueName=%s&startModify=%d&endModify=%d&instType=SWAP&limit=50", p.uniqueName, start, now)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var parsed struct {
		Code string `json:"code"`
		Data []struct {
			InstID   string `json:"instId"`
			PosSide  string `json:"posSide"`
			Side     string `json:"side"`
			AvgPx    string `json:"avgPx"`
			Sz       string `json:"sz"`
			Value    string `json:"value"`
			FillTime string `json:"fillTime"`
			OrdID    string `json:"ordId"`
			Lever    string `json:"lever"`
			TradeCcy string `json:"tradeQuoteCcy"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if parsed.Code != "0" {
		return fmt.Errorf("okx trade-records code=%s msg=%s", parsed.Code, parsed.Msg)
	}
	maxFill := p.tradeCursor
	for _, tr := range parsed.Data {
		ft, _ := strconv.ParseInt(tr.FillTime, 10, 64)
		if ft > maxFill {
			maxFill = ft
		}
		logger.Infof("okx trade log: inst=%s posSide=%s side=%s sz=%s px=%s notional=%s lev=%s", tr.InstID, tr.PosSide, tr.Side, tr.Sz, tr.AvgPx, tr.Value, tr.Lever)
	}
	if maxFill > p.tradeCursor {
		p.tradeCursor = maxFill
	}
	return nil
}
