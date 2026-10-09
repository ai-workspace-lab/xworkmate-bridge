package rolepolicy

import (
	"fmt"
	"strconv"
)

// EffortLevels is the shared effort scale, lowest first. The OpenClaw
// Gateway takes it as chat.send thinking; ACP executors map it in their
// adapters.
var EffortLevels = []string{"off", "low", "medium", "high", "max"}

func effortIndex(level string) int {
	for i, candidate := range EffortLevels {
		if candidate == level {
			return i
		}
	}
	return -1
}

func (r *EffortRange) validate() error {
	start, low, high := effortIndex(r.Start), effortIndex(r.Min), effortIndex(r.Max)
	if start < 0 || low < 0 || high < 0 {
		return fmt.Errorf("start, min and max must be one of %v", EffortLevels)
	}
	if low > start || start > high {
		return fmt.Errorf("needs min <= start <= max, got %s <= %s <= %s", r.Min, r.Start, r.Max)
	}
	return nil
}

func (r *EffortRules) validate() error {
	if r == nil {
		return fmt.Errorf("limits.effort_rules is missing")
	}
	if r.LargeContextTokens <= 0 || r.LargeContextAttachments <= 0 || r.ShortFollowUpChars <= 0 {
		return fmt.Errorf("limits.effort_rules thresholds must be positive")
	}
	return nil
}

// EffortSignals are the request facts the effort rules read. They come from
// the request and the bridge's in-memory session state only.
type EffortSignals struct {
	PromptTokens       int
	Attachments        int
	PromptChars        int
	FollowUp           bool
	PreviousTurnFailed bool
}

// EffortDecision is the resolved effort and how it was reached.
type EffortDecision struct {
	Level       string   `json:"level"`
	Start       string   `json:"start"`
	Min         string   `json:"min"`
	Max         string   `json:"max"`
	Adjustments []string `json:"adjustments,omitempty"`
}

// ResolveEffort starts at the role's start level, applies the rules in a
// fixed order and clamps the result to the role's range. It never calls a
// model.
func ResolveEffort(r EffortRange, rules EffortRules, signals EffortSignals) EffortDecision {
	decision := EffortDecision{Start: r.Start, Min: r.Min, Max: r.Max}
	level := effortIndex(r.Start)
	if signals.PromptTokens > rules.LargeContextTokens || signals.Attachments >= rules.LargeContextAttachments {
		level++
		decision.Adjustments = append(decision.Adjustments,
			"+1 large context: "+strconv.Itoa(signals.PromptTokens)+" tokens, "+strconv.Itoa(signals.Attachments)+" attachments")
	}
	if signals.FollowUp && signals.PromptChars < rules.ShortFollowUpChars {
		level--
		decision.Adjustments = append(decision.Adjustments,
			"-1 short follow-up: "+strconv.Itoa(signals.PromptChars)+" characters")
	}
	if signals.PreviousTurnFailed {
		level++
		decision.Adjustments = append(decision.Adjustments, "+1 retry after a failed turn")
	}
	if low := effortIndex(r.Min); level < low {
		level = low
		decision.Adjustments = append(decision.Adjustments, "clamped to min "+r.Min)
	}
	if high := effortIndex(r.Max); level > high {
		level = high
		decision.Adjustments = append(decision.Adjustments, "clamped to max "+r.Max)
	}
	decision.Level = EffortLevels[level]
	return decision
}
