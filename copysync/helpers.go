package copysync

import (
	"fmt"
	"strconv"
	"strings"
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
