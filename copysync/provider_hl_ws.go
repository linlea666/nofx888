package copysync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"nofx/logger"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// HyperliquidWSProvider 基于 WebSocket 的 HL 领航员事件提供者。
// 仅订阅 userEvents，实时推送 fills；不再使用 HTTP 兜底。
type HyperliquidWSProvider struct {
	address string
	events  chan ProviderEvent
	cursor  int64 // 使用 fill 的 time 作为游标

	conn          *websocket.Conn
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	once          sync.Once
	httpClient    *http.Client
	equityMu      sync.RWMutex
	leaderEquity  float64
	marginModeMap map[string]string
	seenMu        sync.Mutex
	seenTids      map[int64]struct{}
}

type hlWSMessage struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

type hlWSUserData struct {
	Fills []hlFill `json:"fills"`
}

// NewHyperliquidWSProvider 创建 WS Provider
func NewHyperliquidWSProvider(address string) Provider {
	return &HyperliquidWSProvider{
		address:       address,
		events:        make(chan ProviderEvent, 256),
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		marginModeMap: make(map[string]string),
		seenTids:      make(map[int64]struct{}),
	}
}

func (p *HyperliquidWSProvider) Name() string { return "hyperliquid_ws" }

func (p *HyperliquidWSProvider) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.run(ctx)
	}()
	return nil
}

func (p *HyperliquidWSProvider) Stop(ctx context.Context) error {
	p.once.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
	})
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.wg.Wait()
	return nil
}

func (p *HyperliquidWSProvider) Events() <-chan ProviderEvent { return p.events }

func (p *HyperliquidWSProvider) Snapshot(ctx context.Context) (*LeaderState, error) {
	// 使用 HTTP 获取快照，便于对账
	if err := p.refreshClearinghouse(ctx); err != nil {
		return nil, err
	}
	positions := make(map[string]*LeaderPosition)
	p.equityMu.RLock()
	equity := p.leaderEquity
	for coin, mode := range p.marginModeMap {
		positions[coin+"_any"] = &LeaderPosition{
			Symbol:     coin,
			MarginMode: mode,
		}
	}
	p.equityMu.RUnlock()
	return &LeaderState{
		Equity:    equity,
		Positions: positions,
		Timestamp: time.Now(),
	}, nil
}

func (p *HyperliquidWSProvider) GetCursor() int64  { return p.cursor }
func (p *HyperliquidWSProvider) SetCursor(v int64) { p.cursor = v }

func (p *HyperliquidWSProvider) run(ctx context.Context) {
	backoffs := []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}
	attempt := 0
	// 启动前先拉一次净值/模式
	_ = p.refreshClearinghouse(ctx)
	// 低频刷新净值/模式
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
	for {
		if err := p.connectAndSubscribe(); err != nil {
			if attempt >= len(backoffs) {
				attempt = len(backoffs) - 1
			}
			delay := backoffs[attempt]
			logger.Infof("HL WS connect failed: %v, retry in %s", err, delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
				attempt++
				continue
			}
		}
		attempt = 0
		if err := p.readLoop(ctx); err != nil {
			logger.Infof("HL WS read error: %v, reconnecting...", err)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		time.Sleep(1 * time.Second)
	}
}

func (p *HyperliquidWSProvider) connectAndSubscribe() error {
	var dialer websocket.Dialer
	c, _, err := dialer.Dial("wss://api.hyperliquid.xyz/ws", nil)
	if err != nil {
		return err
	}
	p.conn = c
	addr := strings.ToLower(p.address)
	subMsg := map[string]interface{}{
		"method": "subscribe",
		"subscription": map[string]interface{}{
			"type": "userEvents",
			"user": addr,
		},
	}
	if err := p.conn.WriteJSON(subMsg); err != nil {
		return err
	}
	logger.Infof("HL WS subscribed userEvents for %s", addr)
	return nil
}

func (p *HyperliquidWSProvider) readLoop(ctx context.Context) error {
	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()

	p.conn.SetReadLimit(1 << 20)
	p.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	p.conn.SetPongHandler(func(appData string) error {
		p.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pingTicker.C:
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return fmt.Errorf("ping failed: %w", err)
			}
		default:
		}
		_, msg, err := p.conn.ReadMessage()
		if err != nil {
			return err
		}
		p.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		p.handleMessage(msg)
	}
}

func (p *HyperliquidWSProvider) handleMessage(msg []byte) {
	var envelope hlWSMessage
	if err := json.Unmarshal(msg, &envelope); err != nil {
		logger.Infof("HL WS unmarshal failed: %v body=%s", err, string(msg))
		return
	}
	if envelope.Channel != "user" {
		// 订阅响应等，打印一次
		logger.Infof("HL WS recv channel=%s data=%s", envelope.Channel, string(envelope.Data))
		return
	}
	var data hlWSUserData
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		logger.Infof("HL WS user data parse failed: %v body=%s", err, string(envelope.Data))
		return
	}
	if len(data.Fills) == 0 {
		return
	}
	logger.Infof("HL WS fills received: count=%d first_tid=%d", len(data.Fills), data.Fills[0].Tid)
	for _, f := range data.Fills {
		// 过滤已处理游标
		if f.Time <= p.cursor {
			continue
		}
		// tid 去重
		p.seenMu.Lock()
		if _, ok := p.seenTids[f.Tid]; ok {
			p.seenMu.Unlock()
			continue
		}
		p.seenTids[f.Tid] = struct{}{}
		if len(p.seenTids) > 2000 {
			p.seenTids = make(map[int64]struct{}) // 简单裁剪，避免内存膨胀
		}
		p.seenMu.Unlock()

		ev, ok := p.fillToEvent(f)
		if !ok {
			continue
		}
		p.cursor = f.Time
		select {
		case p.events <- ev:
		default:
		}
	}
}

func (p *HyperliquidWSProvider) fillToEvent(f hlFill) (ProviderEvent, bool) {
	price, _ := strconv.ParseFloat(f.Px, 64)
	size, _ := strconv.ParseFloat(f.Sz, 64)
	startPos, _ := strconv.ParseFloat(f.StartPosition, 64)

	action, side := deriveHLActionHL(f.Dir, startPos, size)
	if action == "" || side == "" {
		return ProviderEvent{}, false
	}
	// 游标去重容差，避免重复推送（HL 可能有重复 tid）
	roundedTime := f.Time
	if roundedTime == 0 {
		roundedTime = time.Now().UnixMilli()
	}
	p.equityMu.RLock()
	equity := p.leaderEquity
	marginMode := p.marginModeMap[strings.ToUpper(f.Coin)]
	p.equityMu.RUnlock()
	return ProviderEvent{
		TraceID:      fmt.Sprintf("hl-ws-%d", f.Tid),
		SourceID:     p.address,
		ProviderType: "hyperliquid_ws",
		Symbol:       f.Coin,
		Side:         side,
		Action:       action,
		Price:        price,
		PriceSource:  "fill",
		Size:         size,
		Notional:     price * size,
		LeaderEquity: equity,
		MarginMode:   marginMode,
		Timestamp:    time.UnixMilli(roundedTime),
		Seq:          roundedTime,
	}, true
}

func (p *HyperliquidWSProvider) refreshClearinghouse(ctx context.Context) error {
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

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// 兼容两种返回格式：{"success":true,"data":{...}} 或直接返回 data 对象
	var wrapper struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(rawBytes, &wrapper)

	var data hlClearingData
	if wrapper.Success != nil && len(wrapper.Data) > 0 {
		if *wrapper.Success {
			if err := json.Unmarshal(wrapper.Data, &data); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("hl clearinghouseState not success")
		}
	} else {
		// 尝试直接解析为数据体
		if err := json.Unmarshal(rawBytes, &data); err != nil {
			return fmt.Errorf("hl clearinghouseState decode failed: %w body=%s", err, string(rawBytes))
		}
	}

	if data.MarginSummary == nil {
		return fmt.Errorf("hl clearinghouseState missing margin summary body=%s", string(rawBytes))
	}

	equity, _ := strconv.ParseFloat(data.MarginSummary.AccountValue, 64)
	modeMap := make(map[string]string)
	for _, ap := range data.AssetPositions {
		coin := strings.ToUpper(ap.Position.Coin)
		mode := ap.Position.Leverage.Type
		if mode == "" {
			mode = "cross"
		}
		modeMap[coin] = mode
	}

	p.equityMu.Lock()
	p.leaderEquity = equity
	p.marginModeMap = modeMap
	p.equityMu.Unlock()
	return nil
}
