package copysync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// HyperliquidProvider 通过公开 info 接口轮询 userFills + clearinghouseState。
type HyperliquidProvider struct {
	address    string
	events     chan ProviderEvent
	httpClient *http.Client

	lastTimeMu sync.Mutex
	lastTime   int64 // 上次 userFills 的 time

	leaderEquityMu sync.Mutex
	leaderEquity   float64
}

func NewHyperliquidProvider(address string) *HyperliquidProvider {
	return &HyperliquidProvider{
		address: address,
		events:  make(chan ProviderEvent, 200),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (p *HyperliquidProvider) Name() string { return "hyperliquid_wallet" }

func (p *HyperliquidProvider) Start(ctx context.Context) error {
	go p.pollFills(ctx)
	go p.pollClearinghouse(ctx)
	return nil
}

func (p *HyperliquidProvider) Stop(ctx context.Context) error {
	return nil
}

func (p *HyperliquidProvider) Events() <-chan ProviderEvent { return p.events }

// Snapshot 使用 clearinghouseState 获取当前基线。
func (p *HyperliquidProvider) Snapshot(ctx context.Context) (*LeaderState, error) {
	state, err := p.fetchClearinghouse(ctx)
	if err != nil {
		return nil, err
	}
	positions := make(map[string]*LeaderPosition)
	for _, ap := range state.AssetPositions {
		side := "long"
		if parseFloat(ap.Position.Szi, 0) < 0 {
			side = "short"
		}
		key := fmt.Sprintf("%s_%s", ap.Position.Coin, side)
		positions[key] = &LeaderPosition{
			Symbol:     ap.Position.Coin,
			Side:       side,
			Size:       abs(parseFloat(ap.Position.Szi, 0)),
			EntryPrice: parseFloat(ap.Position.EntryPx, 0),
			MarginUsed: parseFloat(ap.Position.MarginUsed, 0),
			Leverage:   parseFloat(ap.Position.Leverage.Value, 0),
			MarginMode: mapHLLeverageType(ap.Position.Leverage.Type),
		}
	}
	return &LeaderState{
		Equity:    parseFloat(state.MarginSummary.AccountValue, 0),
		Positions: positions,
		Timestamp: time.Now(),
	}, nil
}

func (p *HyperliquidProvider) GetCursor() int64 {
	return p.getLastTime()
}

func (p *HyperliquidProvider) SetCursor(v int64) {
	p.setLastTime(v)
}

func (p *HyperliquidProvider) pollFills(ctx context.Context) {
	interval := 3 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(p.events)

	for {
		if err := p.pullFillsOnce(ctx); err != nil {
			if interval < 15*time.Second {
				interval *= 2
				ticker.Reset(interval)
			}
		} else {
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

func (p *HyperliquidProvider) pollClearinghouse(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		if state, err := p.fetchClearinghouse(ctx); err == nil {
			p.leaderEquityMu.Lock()
			p.leaderEquity = parseFloat(state.MarginSummary.AccountValue, 0)
			p.leaderEquityMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *HyperliquidProvider) pullFillsOnce(ctx context.Context) error {
	payload := map[string]interface{}{
		"type": "userFills",
		"user": p.address,
	}
	reqBody, _ := json.Marshal(payload)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.hyperliquid.xyz/info", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var fills []hlUserFill
	if err := json.NewDecoder(resp.Body).Decode(&fills); err != nil {
		return err
	}

	last := p.getLastTime()
	maxTime := last

	p.leaderEquityMu.Lock()
	leaderEquity := p.leaderEquity
	p.leaderEquityMu.Unlock()

	for _, f := range fills {
		if f.Time <= last {
			continue
		}
		if f.Time > maxTime {
			maxTime = f.Time
		}
		price := parseFloat(f.Px, 0)
		size := parseFloat(f.Sz, 0)
		notional := price * size
		ev := ProviderEvent{
			TraceID:      fmt.Sprintf("hl-%d-%d", f.Oid, f.Time),
			SourceID:     p.address,
			ProviderType: p.Name(),
			Symbol:       f.Coin,
			Side:         mapHLSide(f.Side, f.Dir),
			Action:       mapHLAction(f.Dir),
			Price:        price,
			PriceSource:  "fill",
			Size:         size,
			Notional:     notional,
			Leverage:     0,             // fills 无杠杆，留空
			MarginMode:   "",            // fills 无保证金模式
			MarginUsed:   0,             // fills 无保证金
			LeaderEquity: leaderEquity,  // 可能为 0，caller 可判断
			Timestamp:    time.UnixMilli(f.Time),
			Seq:          f.Time,
		}
		select {
		case p.events <- ev:
		default:
			select {
			case <-p.events:
			default:
			}
			p.events <- ev
		}
	}

	if maxTime > last {
		p.setLastTime(maxTime)
	}
	return nil
}

func (p *HyperliquidProvider) fetchClearinghouse(ctx context.Context) (*hlClearinghouseResp, error) {
	payload := map[string]interface{}{
		"type": "clearinghouseState",
		"user": p.address,
	}
	reqBody, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.hyperliquid.xyz/info", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var state hlClearinghouseResp
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (p *HyperliquidProvider) getLastTime() int64 {
	p.lastTimeMu.Lock()
	defer p.lastTimeMu.Unlock()
	return p.lastTime
}

func (p *HyperliquidProvider) setLastTime(v int64) {
	p.lastTimeMu.Lock()
	defer p.lastTimeMu.Unlock()
	p.lastTime = v
}

// --- HL response structs ---

type hlUserFill struct {
	Coin   string `json:"coin"`
	Px     string `json:"px"`
	Sz     string `json:"sz"`
	Side   string `json:"side"` // A/B
	Dir    string `json:"dir"`  // Open Long / Close Short...
	Time   int64  `json:"time"`
	Oid    int64  `json:"oid"`
	Tid    int64  `json:"tid"`
}

type hlClearinghouseResp struct {
	AssetPositions []struct {
		Position struct {
			Coin        string `json:"coin"`
			Szi         string `json:"szi"`
			EntryPx     string `json:"entryPx"`
			MarginUsed  string `json:"marginUsed"`
			Leverage    struct {
				Value float64 `json:"value"`
				Type  string  `json:"type"`
			} `json:"leverage"`
		} `json:"position"`
	} `json:"assetPositions"`
	MarginSummary struct {
		AccountValue string `json:"accountValue"`
	} `json:"marginSummary"`
}

// --- helper ---
func mapHLSide(side, dir string) string {
	if dir == "Open Long" || dir == "Close Short" {
		return "long"
	}
	if dir == "Open Short" || dir == "Close Long" {
		return "short"
	}
	if side == "B" {
		return "long"
	}
	if side == "A" {
		return "short"
	}
	return side
}

func mapHLAction(dir string) string {
	switch dir {
	case "Open Long", "Open Short":
		return "open"
	case "Close Long", "Close Short":
		return "close"
	default:
		return "open"
	}
}

func mapHLLeverageType(t string) string {
	switch t {
	case "cross":
		return "cross"
	case "isolated":
		return "isolated"
	default:
		return t
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
