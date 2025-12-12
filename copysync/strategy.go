package copysync

import "nofx/logger"

// CopyStrategy defines provider/exchange specific hooks.
type CopyStrategy interface {
	// BeforeEvent runs prior to executing an event.
	// Return skip=true with reason to abort handling.
	BeforeEvent(s *Service, ev ProviderEvent) (skip bool, reason string)
}

// DualSidedStrategy allows simultaneous long/short (default).
type DualSidedStrategy struct{}

func (DualSidedStrategy) BeforeEvent(_ *Service, _ ProviderEvent) (bool, string) {
	return false, ""
}

// SingleSidedStrategy enforces single-direction positions (Hyperliquid).
type SingleSidedStrategy struct{}

func (SingleSidedStrategy) BeforeEvent(s *Service, ev ProviderEvent) (bool, string) {
	if ev.Action == "open" || ev.Action == "add" {
		return handleSingleSidedFollowerPositions(s, ev)
	}
	return false, ""
}

// handleSingleSidedFollowerPositions mimics previous single-sided logic.
func handleSingleSidedFollowerPositions(s *Service, ev ProviderEvent) (bool, string) {
	te, ok := s.executor.(*TraderExecutor)
	if !ok || te == nil || te.Trader == nil {
		return false, ""
	}
	positions, err := te.Trader.GetPositions()
	if err != nil {
		return false, ""
	}
	opposites := []struct {
		side string
		size float64
	}{}
	hasSame := false
	sameSize := 0.0
	for _, p := range positions {
		ps, size, isLong, err := parsePosition(p)
		if err != nil {
			logger.Warnf("copysync: single-sided skip invalid position: %v", err)
			continue
		}
		if ps != ev.Symbol || size <= 0 {
			continue
		}
		if (ev.Side == "long" && isLong) || (ev.Side == "short" && !isLong) {
			hasSame = true
			sameSize += size
		} else {
			side := "short"
			if isLong {
				side = "long"
			}
			opposites = append(opposites, struct {
				side string
				size float64
			}{side: side, size: size})
		}
	}

	// close opposite positions first
	for _, o := range opposites {
		if err := te.close(nil, o.side, ev.Symbol, o.size); err != nil {
			return true, "insufficient_position"
		}
		s.clearTracked(ev.Symbol, o.side)
	}

	if !hasSame {
		return false, ""
	}

	// baseline residual check
	baseSize := 0.0
	if s.baseline != nil && s.baseline.Positions != nil {
		key := symbolSideKey(ev.Symbol, ev.Side)
		if bp := s.baseline.Positions[key]; bp != nil {
			baseSize = bp.Size
		}
	}
	if baseSize <= 0 {
		logger.Warnf("copysync: trim residual same-side position before %s %s size=%.4f", ev.Symbol, ev.Action, sameSize)
		if err := te.close(nil, ev.Side, ev.Symbol, sameSize); err != nil {
			return true, "residual_position_not_cleared"
		}
		s.clearTracked(ev.Symbol, ev.Side)
		return false, ""
	}
	if ev.Action == "open" {
		return true, "same_side_exists"
	}
	return false, ""
}
