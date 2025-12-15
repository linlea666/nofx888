package copysync

import "strings"

// ClassifyErr 统一错误码映射，供决策/订单/日志使用。
func ClassifyErr(msg string) string {
	if msg == "" {
		return ""
	}
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "price_fallback_failed"):
		return "price_fallback_failed"
	case strings.Contains(l, "price_source_down"):
		return "price_source_down"
	case strings.Contains(l, "price_format_zero"):
		return "price_format_zero"
	case strings.Contains(l, "formatted to zero"):
		return "price_format_zero"
	case strings.Contains(l, "exchange_price"):
		return "exchange_price_reject"
	case strings.Contains(l, "reject") && strings.Contains(l, "price"):
		return "exchange_price_reject"
	case strings.Contains(l, "insufficient position"):
		return "insufficient_position"
	case strings.Contains(l, "min qty"):
		return "min_qty_not_met"
	case strings.Contains(l, "min notional"):
		return "min_notional_not_met"
	case strings.Contains(l, "insufficient"):
		return "insufficient_balance"
	case strings.Contains(l, "leader_notional"):
		return "leader_notional_missing"
	case strings.Contains(l, "follower_equity"):
		return "follower_equity_missing"
	case strings.Contains(l, "status_query_failed"):
		return "status_query_failed"
	case strings.Contains(l, "price"):
		return "price_missing"
	case strings.Contains(l, "query_failed"):
		return "status_query_failed"
	case strings.Contains(l, "history_add_no_position"):
		return "follower_position_missing"
	default:
		return "exchange_reject"
	}
}
