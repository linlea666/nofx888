package copysync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HyperliquidAPIProvider 基于官方 info API 的领航员事件提供者。
// 只依赖 userFills / clearinghouseState，不做本地推算。
type HyperliquidAPIProvider struct {
	address       string
	events        chan ProviderEvent
	cursor        int64 // 使用 fill 的 time 作为游标
	httpClient    *http.Client
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	equityMu      sync.RWMutex
	leaderEquity  float64
	marginModeMap map[string]string // coin -> isolated/cross
	leaderPos     map[string]*LeaderPosition
}

type hlFillResp struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
}

type hlFill struct {
	Coin          string  `json:"coin"`
	Px            string  `json:"px"`
	Sz            string  `json:"sz"`
	Side          string  `json:"side"` // A=卖, B=买
	Time          int64   `json:"time"`
	StartPosition string  `json:"startPosition"`
	Dir           string  `json:"dir"`
	ClosedPnl     string  `json:"closedPnl"`
	Hash          string  `json:"hash"`
	Oid           int64   `json:"oid"`
	Crossed       bool    `json:"crossed"`
	Fee           string  `json:"fee"`
	Tid           int64   `json:"tid"`
	Cloid         string  `json:"cloid"`
	FeeToken      string  `json:"feeToken"`
	TwapId        *string `json:"twapId"`
}

type hlClearingResp struct {
	Success bool            `json:"success"`
	Data    *hlClearingData `json:"data"`
}

type hlClearingData struct {
	MarginSummary *struct {
		AccountValue string `json:"accountValue"`
	} `json:"marginSummary"`
	AssetPositions []struct {
		Type     string `json:"type"`
		Position struct {
			Coin     string `json:"coin"`
			Szi      string `json:"szi"`
			Leverage struct {
				Type  string  `json:"type"`
				Value float64 `json:"value"`
			} `json:"leverage"`
			EntryPx       string `json:"entryPx"`
			MarginUsed    string `json:"marginUsed"`
			PositionValue string `json:"positionValue"`
		} `json:"position"`
	} `json:"assetPositions"`
	Time int64 `json:"time"`
}

// NewHyperliquidAPIProvider 创建新的 HL 提供者。
func NewHyperliquidAPIProvider(address string) Provider {
	return &HyperliquidAPIProvider{
		address:       address,
		events:        make(chan ProviderEvent, 256),
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		marginModeMap: make(map[string]string),
		leaderPos:     make(map[string]*LeaderPosition),
	}
}

func (p *HyperliquidAPIProvider) Name() string { return "hyperliquid_api" }

func (p *HyperliquidAPIProvider) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)

	// 初始快照（获取 equity 和杠杆模式）
	_ = p.refreshClearinghouse(ctx)

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
				if err := p.pollFills(ctx); err != nil {
					// 记录但不中断
					continue
				}
			}
		}
	}()

	// 低频刷新账户净值/杠杆模式
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
				_ = p.refreshClearinghouse(ctx)
			}
		}
	}()
	return nil
}

func (p *HyperliquidAPIProvider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	return nil
}

func (p *HyperliquidAPIProvider) Events() <-chan ProviderEvent { return p.events }

func (p *HyperliquidAPIProvider) Snapshot(ctx context.Context) (*LeaderState, error) {
	if err := p.refreshClearinghouse(ctx); err != nil {
		return nil, err
	}
	p.equityMu.RLock()
	defer p.equityMu.RUnlock()

	state := &LeaderState{
		Equity:    p.leaderEquity,
		Positions: make(map[string]*LeaderPosition),
		Timestamp: time.Now(),
	}
	for key, pos := range p.leaderPos {
		cp := *pos
		state.Positions[key] = &cp
	}
	return state, nil
}

func (p *HyperliquidAPIProvider) GetCursor() int64  { return p.cursor }
func (p *HyperliquidAPIProvider) SetCursor(v int64) { p.cursor = v }

func (p *HyperliquidAPIProvider) pollFills(ctx context.Context) error {
	payload := map[string]interface{}{
		"type": "userFills",
		"user": p.address,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.hyperliquid.xyz/info", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var respWrap hlFillResp
	if err := json.NewDecoder(resp.Body).Decode(&respWrap); err != nil {
		return err
	}
	if !respWrap.Success {
		return fmt.Errorf("hl userFills not success")
	}

	var fills []hlFill
	if err := json.Unmarshal(respWrap.Data, &fills); err != nil {
		return err
	}

	for _, f := range fills {
		if f.Time <= p.cursor {
			continue
		}
		ev, ok := p.fillToEvent(f)
		if !ok {
			continue
		}
		p.cursor = f.Time
		select {
		case p.events <- ev:
		default:
			// 队列满丢弃
		}
	}
	return nil
}

func (p *HyperliquidAPIProvider) fillToEvent(f hlFill) (ProviderEvent, bool) {
	price, _ := strconv.ParseFloat(f.Px, 64)
	size, _ := strconv.ParseFloat(f.Sz, 64)
	startPos, _ := strconv.ParseFloat(f.StartPosition, 64)

	action, side := deriveHLActionHL(f.Dir, startPos, size)
	if action == "" || side == "" {
		return ProviderEvent{}, false
	}

	p.equityMu.RLock()
	equity := p.leaderEquity
	marginMode := p.marginModeMap[strings.ToUpper(f.Coin)]
	p.equityMu.RUnlock()

	return ProviderEvent{
		TraceID:      fmt.Sprintf("hl-%d", f.Tid),
		SourceID:     p.address,
		ProviderType: "hyperliquid_api",
		Symbol:       f.Coin,
		Side:         side,
		Action:       action,
		Price:        price,
		PriceSource:  "fill",
		Size:         size,
		Notional:     price * size,
		LeaderEquity: equity,
		MarginMode:   marginMode,
		Timestamp:    time.UnixMilli(f.Time),
		Seq:          f.Time,
	}, true
}

func deriveHLActionHL(dir string, startPos float64, sz float64) (string, string) {
	switch dir {
	case "Open Long":
		if startPos == 0 {
			return "open", "long"
		}
		return "add", "long"
	case "Open Short":
		if startPos == 0 {
			return "open", "short"
		}
		return "add", "short"
	case "Close Long":
		after := startPos - sz
		if math.Abs(after) < 1e-9 {
			return "close", "long"
		}
		return "reduce", "long"
	case "Close Short":
		after := startPos + sz
		if math.Abs(after) < 1e-9 {
			return "close", "short"
		}
		return "reduce", "short"
	default:
		return "", ""
	}
}

func (p *HyperliquidAPIProvider) refreshClearinghouse(ctx context.Context) error {
	payload := map[string]interface{}{
		"type": "clearinghouseState",
		"user": p.address,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.hyperliquid.xyz/info", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var respWrap hlClearingResp
	_ = json.Unmarshal(bodyBytes, &respWrap)

	// 如果返回没有 success 包装，尝试直接解析为数据体
	if respWrap.Data == nil {
		var direct hlClearingData
		if err := json.Unmarshal(bodyBytes, &direct); err == nil && (direct.MarginSummary != nil || len(direct.AssetPositions) > 0) {
			respWrap.Data = &direct
			respWrap.Success = true
		}
	}

	if !respWrap.Success || respWrap.Data == nil {
		return fmt.Errorf("hl clearinghouseState not success: %s", string(bodyBytes))
	}

	equity := p.leaderEquity
	if respWrap.Data.MarginSummary != nil {
		if val, err := strconv.ParseFloat(respWrap.Data.MarginSummary.AccountValue, 64); err == nil {
			equity = val
		}
	}

	modeMap := make(map[string]string, len(respWrap.Data.AssetPositions))
	positions := make(map[string]*LeaderPosition)
	for _, ap := range respWrap.Data.AssetPositions {
		coin := strings.ToUpper(ap.Position.Coin)
		mode := ap.Position.Leverage.Type
		if mode == "" {
			mode = "cross"
		}
		modeMap[coin] = mode

		szi, _ := strconv.ParseFloat(ap.Position.Szi, 64)
		if math.Abs(szi) < 1e-12 {
			continue
		}
		side := "long"
		size := szi
		if szi < 0 {
			side = "short"
			size = math.Abs(szi)
		}
		entry, _ := strconv.ParseFloat(ap.Position.EntryPx, 64)
		marginUsed, _ := strconv.ParseFloat(ap.Position.MarginUsed, 64)

		key := fmt.Sprintf("%s_%s", coin, side)
		positions[key] = &LeaderPosition{
			Symbol:     coin,
			Side:       side,
			Size:       size,
			EntryPrice: entry,
			MarginUsed: marginUsed,
			Leverage:   ap.Position.Leverage.Value,
			MarginMode: mode,
		}
	}

	p.equityMu.Lock()
	p.leaderEquity = equity
	p.marginModeMap = modeMap
	p.leaderPos = positions
	p.equityMu.Unlock()
	return nil
}
