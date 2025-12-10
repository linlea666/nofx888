package copysync

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

	conn   *websocket.Conn
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
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
		address: address,
		events:  make(chan ProviderEvent, 256),
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
	// WS 模式不提供快照，返回空
	return &LeaderState{Equity: 0, Positions: make(map[string]*LeaderPosition), Timestamp: time.Now()}, nil
}

func (p *HyperliquidWSProvider) GetCursor() int64  { return p.cursor }
func (p *HyperliquidWSProvider) SetCursor(v int64) { p.cursor = v }

func (p *HyperliquidWSProvider) run(ctx context.Context) {
	for {
		if err := p.connectAndSubscribe(); err != nil {
			logger.Infof("HL WS connect failed: %v, retry in 2s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}
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
		LeaderEquity: 0,  // WS 未返回净值，需依赖外部净值获取（可选）
		MarginMode:   "", // WS 未返回模式，低频用 HTTP 或外部注入
		Timestamp:    time.UnixMilli(roundedTime),
		Seq:          roundedTime,
	}, true
}

// deriveHLAction 复用 dir/startPosition 逻辑
func deriveHLAction(dir string, startPos float64, sz float64) (string, string) {
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
