package copysync

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

// handleSingleSidedFollowerPositions ensures follower has no opposite side before processing.
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
	for _, p := range positions {
		ps, size, isLong, err := parsePosition(p)
		if err != nil {
			continue
		}
		if !SymbolsMatch(ps, ev.Symbol) || size <= 0 {
			continue
		}
		if (ev.Side == "long" && isLong) || (ev.Side == "short" && !isLong) {
			continue
		}
		side := "short"
		if isLong {
			side = "long"
		}
		opposites = append(opposites, struct {
			side string
			size float64
		}{side: side, size: size})
	}

	// Close opposite positions first; skip this event on close failure.
	for _, o := range opposites {
		if err := te.close(nil, o.side, ev.Symbol, o.size); err != nil {
			return true, "insufficient_position"
		}
		s.clearTracked(ev.Symbol, o.side)
	}

	return false, ""
}
