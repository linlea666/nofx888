package copysync

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	symbolNormalizeReplacer = strings.NewReplacer("-", "", "_", "", "/", "", " ", "", ":", "", ".", "")
	symbolQuoteSuffixes     = []string{"USDTPERP", "USDT", "USDC", "BUSD", "USD", "PERP", "SWAP", "FUT"}
)

func parseFloat(s string, def float64) float64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// NormalizeSymbol strips common quote suffixes/formatting so that BTC/BTCPERP/BTCUSDT map to BTC.
func NormalizeSymbol(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s == "" {
		return ""
	}
	s = symbolNormalizeReplacer.Replace(s)
	orig := s
	for {
		trimmed := false
		for _, suf := range symbolQuoteSuffixes {
			if len(s) > len(suf) && strings.HasSuffix(s, suf) {
				s = strings.TrimSuffix(s, suf)
				trimmed = true
				break
			}
		}
		if !trimmed {
			break
		}
	}
	if s == "" {
		return orig
	}
	return s
}

// SymbolsMatch compares two symbols after running NormalizeSymbol.
func SymbolsMatch(a, b string) bool {
	na := NormalizeSymbol(a)
	nb := NormalizeSymbol(b)
	if na == "" || nb == "" {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	return na == nb
}

type actionFragment struct {
	action string
	side   string
	qty    float64
}

func decomposeClose(startPos float64, delta float64, closingSide string) []actionFragment {
	fragments := []actionFragment{}
	if delta <= 0 {
		return fragments
	}
	existing := math.Abs(startPos)
	closeQty := math.Min(existing, delta)
	if closeQty <= 0 {
		return fragments
	}
	act := "close"
	if closeQty < existing {
		act = "reduce"
	}
	fragments = append(fragments, actionFragment{
		action: act,
		side:   closingSide,
		qty:    closeQty,
	})
	remaining := delta - existing
	if remaining > 1e-9 {
		openSide := "short"
		if closingSide == "short" {
			openSide = "long"
		}
		fragments = append(fragments, actionFragment{
			action: "open",
			side:   openSide,
			qty:    remaining,
		})
	}
	return fragments
}

func deriveHLFragments(dir string, startPos float64, sz float64) []actionFragment {
	switch dir {
	case "Open Long":
		act := "add"
		if math.Abs(startPos) < 1e-9 {
			act = "open"
		}
		return []actionFragment{{action: act, side: "long", qty: sz}}
	case "Open Short":
		act := "add"
		if math.Abs(startPos) < 1e-9 {
			act = "open"
		}
		return []actionFragment{{action: act, side: "short", qty: sz}}
	case "Close Long":
		return decomposeClose(startPos, sz, "long")
	case "Close Short":
		return decomposeClose(startPos, sz, "short")
	default:
		return nil
	}
}

// parsePosition 通用持仓解析：返回 symbol、绝对数量、是否多头。
func parsePosition(p map[string]interface{}) (symbol string, size float64, isLong bool, err error) {
	if p == nil {
		return "", 0, false, fmt.Errorf("position_nil")
	}
	if ps, ok := p["symbol"].(string); ok {
		symbol = ps
	}
	if ps, ok := p["instId"].(string); ok && symbol == "" {
		symbol = ps
	}
	if ps, ok := p["instrument_id"].(string); ok && symbol == "" {
		symbol = ps
	}
	// 方向优先用 posSide/positionSide
	if ps, ok := p["posSide"].(string); ok && ps != "" {
		switch strings.ToLower(ps) {
		case "long":
			isLong = true
		case "short":
			isLong = false
		}
	}
	if ps, ok := p["positionSide"].(string); ok && ps != "" && !isLong {
		switch strings.ToLower(ps) {
		case "long":
			isLong = true
		case "short":
			isLong = false
		}
	}
	if ps, ok := p["side"].(string); ok && ps != "" && symbol == "" {
		switch strings.ToLower(ps) {
		case "long", "buy":
			isLong = true
		case "short", "sell":
			isLong = false
		}
	}
	switch v := p["positionAmt"].(type) {
	case string:
		size, _ = strconv.ParseFloat(v, 64)
	case float64:
		size = v
	}
	if size == 0 {
		switch v := p["size"].(type) {
		case string:
			size, _ = strconv.ParseFloat(v, 64)
		case float64:
			size = v
		}
	}
	if size == 0 {
		switch v := p["availPos"].(type) {
		case string:
			size, _ = strconv.ParseFloat(v, 64)
		case float64:
			size = v
		}
	}
	if size != 0 && !isLong {
		isLong = size > 0
	}
	if size < 0 {
		size = -size
	}
	if symbol == "" || size == 0 {
		err = fmt.Errorf("position_parse_failed symbol=%s size=%.4f", symbol, size)
	}
	return
}
