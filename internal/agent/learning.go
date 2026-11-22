package agent

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
)

// ErrAnalyticsWindow is AgentPulse answering for a longer window than the
// learning agent asked for: its counts would include signals older than
// AgentPulse's 30-day threshold.
var ErrAnalyticsWindow = errors.New("feedback analytics cover more than the learning window")

// FindPatterns is the learning agent's aggregation (M11-02): deterministic Go
// code, with no model call. For each event type and label in AgentPulse's
// feedback analytics, it computes the dismiss and mute ratios and keeps the
// groups that meet AgentPulse's thresholds:
//
//   - the analytics start no earlier than windowStart, the 30-day window
//     (domain.LearningWindow); otherwise it returns ErrAnalyticsWindow;
//   - at least 5 signals (domain.LearningMinSignals), counting dismissed and
//     muted, the signals a suggestion to lower a score rests on;
//   - from at least 2 users (domain.LearningMinUsers);
//   - and, Taskify's own rule, at least half the deliveries dismissed or
//     muted (domain.LearningMinNegativeRatio), so the pattern is
//     consistent rather than the noise of a busy event type.
//
// Ratios are shares of the deliveries, or of the signals when AgentPulse
// counted fewer deliveries than signals. The patterns are ordered by signal
// count, then by the share dismissed or muted, then by event type and label
// (no label first), and have no ID yet. A scope listed twice counts once,
// as first listed.
func FindPatterns(a domain.FeedbackAnalytics, windowStart time.Time) ([]domain.LearningPattern, error) {
	if a.Since.IsZero() || a.Since.Before(windowStart.Truncate(time.Second)) {
		return nil, fmt.Errorf("%w: asked from %s, answered from %s", ErrAnalyticsWindow,
			windowStart.UTC().Format(time.RFC3339), a.Since.UTC().Format(time.RFC3339))
	}
	var out []domain.LearningPattern
	var seen []domain.EventPattern
	for _, g := range a.Groups {
		scope := g.Pattern()
		if g.EventType == "" || slices.ContainsFunc(seen, scope.Equal) {
			continue
		}
		seen = append(seen, scope)
		negative := g.Dismissed + g.Muted
		base := max(g.Delivered, g.Acted+g.Dismissed+g.Muted+g.Snoozed)
		if base == 0 || negative < domain.LearningMinSignals || g.DistinctUsers < domain.LearningMinUsers ||
			float64(negative)/float64(base) < domain.LearningMinNegativeRatio {
			continue
		}
		out = append(out, domain.LearningPattern{
			EventType: g.EventType, Label: cloneLabel(g.Label),
			Delivered: g.Delivered, Acted: g.Acted, Dismissed: g.Dismissed, Muted: g.Muted, Snoozed: g.Snoozed,
			DistinctUsers: g.DistinctUsers, SignalCount: negative,
			DismissRatio: ratio(g.Dismissed, base), MuteRatio: ratio(g.Muted, base),
		})
	}
	slices.SortStableFunc(out, func(a, b domain.LearningPattern) int {
		return cmp.Or(
			cmp.Compare(b.SignalCount, a.SignalCount),
			cmp.Compare(b.DismissRatio+b.MuteRatio, a.DismissRatio+a.MuteRatio),
			strings.Compare(a.EventType, b.EventType),
			compareLabels(a.Label, b.Label),
		)
	})
	return out, nil
}

func ratio(n, base int) float64 {
	return math.Round(float64(n)/float64(base)*1000) / 1000
}

// compareLabels orders no label first, then labels by byte order.
func compareLabels(a, b *string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return strings.Compare(*a, *b)
}

func cloneLabel(l *string) *string {
	if l == nil {
		return nil
	}
	v := *l
	return &v
}
