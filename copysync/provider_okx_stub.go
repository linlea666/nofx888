package copysync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// OKXProvider 通过 OKX 公共 copy trading 接口轮询领航员成交/仓位/资产。
// 仅使用 GET 接口，无需私钥。
type OKXProvider struct {
	uniqueName string
	events     chan ProviderEvent
	httpClient *http.Client

	lastModifyMu sync.Mutex
	lastModify   int64 // 上次 trade-records 的 uTime/startModify

	leaderEquityMu sync.Mutex
	leaderEquity   float64 // USDT
}

func NewOKXProvider(uniqueName string) *OKXProvider {
	return &OKXProvider{
		uniqueName: uniqueName,
		events:     make(chan ProviderEvent, 200),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *OKXProvider) Name() string { return "okx_wallet" }

func (p *OKXProvider) Start(ctx context.Context) error {
	go p.pollTrades(ctx)
	go p.pollAssets(ctx)
	return nil
}

func (p *OKXProvider) Stop(ctx context.Context) error {
	// nothing to cleanup
	return nil
}

func (p *OKXProvider) Events() <-chan ProviderEvent { return p.events }

// Snapshot 使用 position-current + asset 获取当前基线。
func (p *OKXProvider) Snapshot(ctx context.Context) (*LeaderState, error) {
	equity, _ := p.fetchEquity(ctx)
	positions, _ := p.fetchPositions(ctx)
	return &LeaderState{
		Equity:    equity,
		Positions: positions,
		Timestamp: time.Now(),
	}, nil
}

func (p *OKXProvider) GetCursor() int64 {
	return p.getLastModify()
}

func (p *OKXProvider) SetCursor(v int64) {
	p.setLastModify(v)
}

// pollTrades 轮询 trade-records，生成 ProviderEvent。
func (p *OKXProvider) pollTrades(ctx context.Context) {
	interval := 3 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(p.events)

	for {
		if err := p.pullOnce(ctx); err != nil {
			// 简单退避：错误时将间隔翻倍，最多 15s
			if interval < 15*time.Second {
				interval *= 2
				ticker.Reset(interval)
			}
		} else {
			// 成功后恢复默认间隔
			if interval != 3*time.Second {
				interval = 3 * time.Second
				ticker.Reset(interval)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *OKXProvider) pullOnce(ctx context.Context) error {
	end := time.Now().Add(10 * time.Second).UnixMilli()
	start := p.getLastModify()
	if start == 0 {
		start = end - 3*60*1000 // 初次取最近3分钟
	}

	url := fmt.Sprintf("https://www.okx.com/priapi/v5/ecotrade/public/community/user/trade-records?uniqueName=%s&startModify=%d&endModify=%d&limit=80",
		p.uniqueName, start, end)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var trResp okxTradeRecordsResp
	if err := json.NewDecoder(resp.Body).Decode(&trResp); err != nil {
		return err
	}
	if trResp.Code != "0" {
		return fmt.Errorf("okx trade-records code=%s msg=%s", trResp.Code, trResp.Msg)
	}

	maxModify := start
	p.leaderEquityMu.Lock()
	leaderEquity := p.leaderEquity
	p.leaderEquityMu.Unlock()

	for _, d := range trResp.Data {
		ut := parseInt64(d.UTime)
		if ut <= start {
			continue
		}
		if ut > maxModify {
			maxModify = ut
		}
		ev := ProviderEvent{
			TraceID:      fmt.Sprintf("okx-%s-%d", d.OrdID, ut),
			SourceID:     p.uniqueName,
			ProviderType: p.Name(),
			Symbol:       d.InstID,
			Side:         mapSide(d.Side, d.PosSide),
			Action:       mapAction(d.Side, d.PosSide),
			Price:        parseFloat(d.AvgPx, parseFloat(d.Px, 0)),
			PriceSource:  "fill",
			Size:         parseFloat(d.Sz, 0),
			Notional:     parseFloat(d.Value, 0),
			Leverage:     parseFloat(d.Lever, 0),
			MarginMode:   mapMarginMode(d.MgnMode),
			MarginUsed:   0, // trade-records 无保证金，留空
			LeaderEquity: leaderEquity,
			Timestamp:    time.UnixMilli(ut),
			Seq:          ut,
		}
		select {
		case p.events <- ev:
		default:
			// 防止阻塞，丢弃最旧
			select {
			case <-p.events:
			default:
			}
			p.events <- ev
		}
	}

	if maxModify > start {
		p.setLastModify(maxModify)
	}
	return nil
}

// pollAssets 定期更新领航员净值（USDT）。
func (p *OKXProvider) pollAssets(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		if equity, err := p.fetchEquity(ctx); err == nil && equity > 0 {
			p.leaderEquityMu.Lock()
			p.leaderEquity = equity
			p.leaderEquityMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *OKXProvider) fetchEquity(ctx context.Context) (float64, error) {
	url := fmt.Sprintf("https://www.okx.com/priapi/v5/ecotrade/public/community/user/asset?uniqueName=%s", p.uniqueName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var assetResp okxAssetResp
	if err := json.NewDecoder(resp.Body).Decode(&assetResp); err != nil {
		return 0, err
	}
	if assetResp.Code != "0" {
		return 0, fmt.Errorf("okx asset code=%s msg=%s", assetResp.Code, assetResp.Msg)
	}
	for _, row := range assetResp.Data {
		if row.Currency == "USDT" {
			return parseFloat(row.Amount, 0), nil
		}
	}
	return 0, fmt.Errorf("okx asset: USDT not found")
}

func (p *OKXProvider) fetchPositions(ctx context.Context) (map[string]*LeaderPosition, error) {
	url := fmt.Sprintf("https://www.okx.com/priapi/v5/ecotrade/public/community/user/position-current?uniqueName=%s", p.uniqueName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var posResp okxPositionResp
	if err := json.NewDecoder(resp.Body).Decode(&posResp); err != nil {
		return nil, err
	}
	if posResp.Code != "0" {
		return nil, fmt.Errorf("okx position code=%s msg=%s", posResp.Code, posResp.Msg)
	}
	result := make(map[string]*LeaderPosition)
	for _, wrapper := range posResp.Data {
		for _, pos := range wrapper.PosData {
			side := mapPosSide(pos.PosSide)
			key := fmt.Sprintf("%s_%s", pos.InstID, side)
			result[key] = &LeaderPosition{
				Symbol:     pos.InstID,
				Side:       side,
				Size:       parseFloat(pos.Pos, 0),
				EntryPrice: parseFloat(pos.AvgPx, 0),
				MarginUsed: parseFloat(pos.Margin, 0),
				Leverage:   parseFloat(pos.Lever, 0),
				MarginMode: mapMarginMode(pos.MgnMode),
			}
		}
	}
	return result, nil
}

func (p *OKXProvider) getLastModify() int64 {
	p.lastModifyMu.Lock()
	defer p.lastModifyMu.Unlock()
	return p.lastModify
}

func (p *OKXProvider) setLastModify(v int64) {
	p.lastModifyMu.Lock()
	defer p.lastModifyMu.Unlock()
	p.lastModify = v
}

// --- OKX response structs ---

type okxTradeRecordsResp struct {
	Code string            `json:"code"`
	Msg  string            `json:"msg"`
	Data []okxTradeRecords `json:"data"`
}

type okxTradeRecords struct {
	InstID   string `json:"instId"`
	InstType string `json:"instType"`
	Side     string `json:"side"`
	PosSide  string `json:"posSide"`
	AvgPx    string `json:"avgPx"`
	Px       string `json:"px"`
	Sz       string `json:"sz"`
	Value    string `json:"value"`
	Lever    string `json:"lever"`
	MgnMode  string `json:"mgnMode"`
	OrdID    string `json:"ordId"`
	UTime    string `json:"uTime"`
}

type okxAssetResp struct {
	Code string        `json:"code"`
	Msg  string        `json:"msg"`
	Data []okxAssetRow `json:"data"`
}

type okxAssetRow struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

type okxPositionResp struct {
	Code string               `json:"code"`
	Msg  string               `json:"msg"`
	Data []okxPositionWrapper `json:"data"`
}

type okxPositionWrapper struct {
	PosData []okxPosition `json:"posData"`
}

type okxPosition struct {
	InstID  string `json:"instId"`
	PosSide string `json:"posSide"`
	Pos     string `json:"pos"`
	AvgPx   string `json:"avgPx"`
	Margin  string `json:"margin"`
	Lever   string `json:"lever"`
	MgnMode string `json:"mgnMode"`
}

func mapPosSide(posSide string) string {
	switch posSide {
	case "long", "net", "LONG":
		return "long"
	case "short", "SHORT":
		return "short"
	default:
		return "net"
	}
}

func mapSide(side, posSide string) string {
	// 保持简单映射：买=long，卖=short，若 posSide 给定则优先
	ps := mapPosSide(posSide)
	if ps != "net" {
		return ps
	}
	if side == "buy" {
		return "long"
	}
	if side == "sell" {
		return "short"
	}
	return side
}

func mapAction(side, posSide string) string {
	ps := mapPosSide(posSide)
	if ps == "long" {
		if side == "buy" {
			return "open"
		}
		return "reduce"
	}
	if ps == "short" {
		if side == "sell" {
			return "open"
		}
		return "reduce"
	}
	// net 模式未知，默认 open
	return "open"
}

func mapMarginMode(m string) string {
	switch m {
	case "cross":
		return "cross"
	case "isolated":
		return "isolated"
	default:
		return m
	}
}
