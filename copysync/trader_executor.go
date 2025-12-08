package copysync

import (
	"context"
	"fmt"
	"nofx/logger"
	"nofx/store"
	"nofx/trader"
	"strconv"
	"time"
)

// TraderExecutor 将 CopyDecision 映射到 trader.Trader 下单/平仓接口。
// 注意：精度/最小单校验依赖交易所自身，若需前置校验可在此补充。
type TraderExecutor struct {
	Trader        trader.Trader
	Config        CopyConfig
	EnableLeverageSync bool
	EnableMarginSync   bool
	// 可选的本地限额提示（不阻断）
	MaxLeverageHint int
	OnResult        func(decision *CopyDecision, err error)

	// 可选的订单日志写入（如果需要持久化到 trader_orders）
	OrderLogger func(order *store.TraderOrder, dec *CopyDecision, execErr error)
}

// ExecResult 用于记录执行结果，便于上层日志。
type ExecResult struct {
	Qty         float64
	FormattedQty float64
	OrderID     interface{}
	SkipReason  string
}

func (e *TraderExecutor) ExecuteCopy(ctx context.Context, decision *CopyDecision) error {
	if e.Trader == nil {
		return fmt.Errorf("copysync: trader is nil")
	}

	// 杠杆/保证金模式同步（尽力而为）
	if e.EnableMarginSync && decision.ProviderEvent.MarginMode != "" {
		if err := e.Trader.SetMarginMode(decision.ProviderEvent.Symbol, decision.ProviderEvent.MarginMode == "cross"); err != nil {
			logger.Infof("copysync: margin mode sync failed %v", err)
		}
	}
	if e.EnableLeverageSync && decision.ProviderEvent.Leverage > 0 {
		if err := e.Trader.SetLeverage(decision.ProviderEvent.Symbol, int(decision.ProviderEvent.Leverage)); err != nil {
			logger.Infof("copysync: leverage sync failed %v", err)
		}
	}

	side := decision.ProviderEvent.Side
	qty := decision.FollowerQty
	if qty <= 0 {
		return fmt.Errorf("copysync: qty <= 0")
	}

	formattedQty := qty
	// 使用交易所 FormatQuantity 进行精度/最小量矫正
	if formatted, err := e.Trader.FormatQuantity(decision.ProviderEvent.Symbol, qty); err == nil && formatted != "" {
		if v, err2 := strconv.ParseFloat(formatted, 64); err2 == nil {
			if v <= 0 {
				return fmt.Errorf("copysync: qty formatted to zero (min qty not met)")
			}
			formattedQty = v
			decision.FollowerQty = formattedQty
		}
	}

	switch decision.ProviderEvent.Action {
	case "open", "add":
		// 本地杠杆提示（不阻断）
		if e.MaxLeverageHint > 0 && decision.ProviderEvent.Leverage > float64(e.MaxLeverageHint) {
			logger.Infof("copysync: leverage %.2f exceeds local hint %d (not blocking)", decision.ProviderEvent.Leverage, e.MaxLeverageHint)
		}
		err := e.open(decision, side, decision.ProviderEvent.Symbol, formattedQty, decision.ProviderEvent.Leverage)
		if e.OnResult != nil {
			e.OnResult(decision, err)
		}
		return err
	case "reduce", "close":
		err := e.close(decision, side, decision.ProviderEvent.Symbol, formattedQty)
		if e.OnResult != nil {
			e.OnResult(decision, err)
		}
		return err
	default:
		return fmt.Errorf("copysync: unknown action %s", decision.ProviderEvent.Action)
	}
}

func (e *TraderExecutor) open(dec *CopyDecision, side, symbol string, qty float64, lev float64) error {
	switch side {
	case "long":
		order, err := e.Trader.OpenLong(symbol, qty, int(lev))
		e.logOrder(dec, symbol, side, "open_long", qty, lev, order, err)
		return err
	case "short":
		order, err := e.Trader.OpenShort(symbol, qty, int(lev))
		e.logOrder(dec, symbol, side, "open_short", qty, lev, order, err)
		return err
	default:
		return fmt.Errorf("copysync: unknown side %s", side)
	}
}

func (e *TraderExecutor) close(dec *CopyDecision, side, symbol string, qty float64) error {
	// 防超量平仓：读取当前持仓截断
	available := qty
	if positions, err := e.Trader.GetPositions(); err == nil {
		for _, p := range positions {
			ps, _ := p["symbol"].(string)
			sideRaw, _ := p["side"].(string)
			sizeVal := 0.0
			switch v := p["positionAmt"].(type) {
			case string:
				sizeVal, _ = strconv.ParseFloat(v, 64)
			case float64:
				sizeVal = v
			}
			if ps == symbol && sizeVal != 0 {
				long := sizeVal > 0
				if (long && side == "long") || (!long && side == "short") || sideRaw == side {
					if sizeVal < 0 {
						sizeVal = -sizeVal
					}
					if sizeVal < available {
						available = sizeVal
					}
				}
			}
		}
	}
	if available <= 0 {
		return fmt.Errorf("insufficient_position")
	}

	switch side {
	case "long":
		order, err := e.Trader.CloseLong(symbol, available)
		e.logOrder(dec, symbol, side, "close_long", qty, 0, order, err)
		return err
	case "short":
		order, err := e.Trader.CloseShort(symbol, available)
		e.logOrder(dec, symbol, side, "close_short", qty, 0, order, err)
		return err
	default:
		return fmt.Errorf("copysync: unknown side %s", side)
	}
}

// logOrder 写入订单日志（如果配置了 logger）。
func (e *TraderExecutor) logOrder(dec *CopyDecision, symbol, side, action string, qty float64, lev float64, order map[string]interface{}, err error) {
	if e.OrderLogger == nil {
		return
	}
	o := &store.TraderOrder{
		Symbol:       symbol,
		Side:         side,
		Action:       action,
		Quantity:     qty,
		Leverage:     int(lev),
		Status:       "NEW",
		ProviderType: e.Config.ProviderType,
		CopyRatio:    e.Config.CopyRatio,
	}
	if dec != nil {
		o.TraceID = dec.ProviderEvent.TraceID
		o.ProviderType = dec.ProviderEvent.ProviderType
		o.LeaderPrice = dec.ProviderEvent.Price
		o.LeaderNotional = dec.ProviderEvent.Notional
		o.PriceSource = dec.PriceSource
		if o.Price == 0 {
			o.Price = dec.Price
		}
		if dec.ErrCode != "" {
			o.ErrCode = dec.ErrCode
		}
		if dec.MinNotionalHit || dec.MaxNotionalHit {
			o.SkipReason = fmt.Sprintf("min_hit=%v,max_hit=%v", dec.MinNotionalHit, dec.MaxNotionalHit)
		}
	}
	if order != nil {
		if id, ok := order["orderId"]; ok {
			o.OrderID = fmt.Sprintf("%v", id)
		}
		if clientId, ok := order["clientOrderId"]; ok {
			o.ClientOrderID = fmt.Sprintf("%v", clientId)
		}
		if price, ok := order["price"].(float64); ok {
			o.Price = price
		} else if avg, ok := order["avgPrice"].(float64); ok {
			o.Price = avg
		}
	}
	if o.OrderID == "" {
		if dec != nil && dec.ProviderEvent.TraceID != "" {
			o.OrderID = dec.ProviderEvent.TraceID
		} else {
			o.OrderID = fmt.Sprintf("%s-%d", symbol, time.Now().UnixNano())
		}
	}
	if err != nil {
		o.Status = "ERROR"
		if o.SkipReason != "" {
			o.SkipReason = o.SkipReason + ";" + err.Error()
		} else {
			o.SkipReason = err.Error()
		}
		if o.ErrCode == "" {
			o.ErrCode = ClassifyErr(err.Error())
		}
	}
	e.OrderLogger(o, dec, err)
}
