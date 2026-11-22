package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
)

func label(s string) *string { return &s }

// describePatterns renders patterns for comparison: scope, signals, ratios.
func describePatterns(ps []domain.LearningPattern) string {
	var lines []string
	for _, p := range ps {
		lines = append(lines, fmt.Sprintf("%s: %d signals, dismiss %v, mute %v", p.Pattern(), p.SignalCount, p.DismissRatio, p.MuteRatio))
	}
	return strings.Join(lines, "\n")
}

// TestFindPatternsFindsAConsistentlyDismissedLabel is M11-02: in a history
// where comments on docs tasks are dismissed again and again by several
// people, the docs label is a pattern; labels people act on are not.
func TestFindPatternsFindsAConsistentlyDismissedLabel(t *testing.T) {
	windowStart := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	a := domain.FeedbackAnalytics{Since: windowStart, Groups: []domain.FeedbackGroup{
		{EventType: "task.comment_added", Label: label("backend"), Delivered: 40, Acted: 25, Dismissed: 3, DistinctUsers: 6},
		{EventType: "task.comment_added", Label: label("docs"), Delivered: 20, Acted: 1, Dismissed: 14, Muted: 2, DistinctUsers: 4},
		{EventType: "task.assigned", Label: label("docs"), Delivered: 6, Acted: 6, DistinctUsers: 3},
	}}
	got, err := FindPatterns(a, windowStart)
	if err != nil {
		t.Fatal(err)
	}
	if want := "task.comment_added, label docs: 16 signals, dismiss 0.7, mute 0.1"; describePatterns(got) != want {
		t.Fatalf("patterns:\n%s\nwant:\n%s", describePatterns(got), want)
	}
	p := got[0]
	if p.Delivered != 20 || p.Acted != 1 || p.Dismissed != 14 || p.Muted != 2 || p.DistinctUsers != 4 || p.ID != "" ||
		p.CurrentAdjustment != 0 {
		t.Errorf("pattern = %+v", p)
	}
	// The pattern does not share the analytics' label.
	*a.Groups[1].Label = "changed"
	if *p.Label != "docs" {
		t.Error("the pattern shares its label with the input")
	}
}

// TestFindPatternsDropsWhatIsBelowAThreshold: below 5 signals, 2 users, or a
// consistent share, or outside the 30-day window, nothing is proposed.
func TestFindPatternsDropsWhatIsBelowAThreshold(t *testing.T) {
	windowStart := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	for name, g := range map[string]domain.FeedbackGroup{
		"four signals":            {EventType: "task.updated", Label: label("docs"), Delivered: 4, Dismissed: 3, Muted: 1, DistinctUsers: 3},
		"one user":                {EventType: "task.updated", Label: label("docs"), Delivered: 12, Dismissed: 12, DistinctUsers: 1},
		"no users":                {EventType: "task.updated", Label: label("docs"), Delivered: 12, Dismissed: 12},
		"under half":              {EventType: "task.updated", Label: label("docs"), Delivered: 100, Dismissed: 30, Muted: 19, DistinctUsers: 9},
		"snoozes are not signals": {EventType: "task.updated", Label: label("docs"), Delivered: 10, Dismissed: 4, Snoozed: 6, DistinctUsers: 3},
		"nothing delivered":       {EventType: "task.updated", Label: label("docs"), DistinctUsers: 3},
		"no event type":           {Label: label("docs"), Delivered: 10, Dismissed: 10, DistinctUsers: 3},
	} {
		got, err := FindPatterns(domain.FeedbackAnalytics{Since: windowStart, Groups: []domain.FeedbackGroup{g}}, windowStart)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: %s, %v", name, describePatterns(got), err)
		}
	}

	// Exactly at every threshold is a pattern.
	edge := domain.FeedbackGroup{EventType: "task.updated", Delivered: 10, Dismissed: 4, Muted: 1, DistinctUsers: 2}
	if got, err := FindPatterns(domain.FeedbackAnalytics{Since: windowStart, Groups: []domain.FeedbackGroup{edge}}, windowStart); err != nil ||
		describePatterns(got) != "task.updated, no label: 5 signals, dismiss 0.4, mute 0.1" {
		t.Errorf("at the thresholds: %s, %v", describePatterns(got), err)
	}

	// Counts from before the window are not used: an answer for a longer
	// window is refused, as is one that does not say when it starts.
	strong := domain.FeedbackGroup{EventType: "task.updated", Label: label("docs"), Delivered: 50, Dismissed: 50, DistinctUsers: 9}
	for name, since := range map[string]time.Time{"31 days": windowStart.Add(-24 * time.Hour), "no since": {}} {
		got, err := FindPatterns(domain.FeedbackAnalytics{Since: since, Groups: []domain.FeedbackGroup{strong}}, windowStart)
		if !errors.Is(err, ErrAnalyticsWindow) || len(got) != 0 {
			t.Errorf("%s: %s, %v", name, describePatterns(got), err)
		}
	}
	// A window start with sub-second precision is asked for to the second.
	if _, err := FindPatterns(domain.FeedbackAnalytics{Since: windowStart, Groups: []domain.FeedbackGroup{strong}},
		windowStart.Add(400*time.Millisecond)); err != nil {
		t.Errorf("same second: %v", err)
	}
}

// TestFindPatternsOrder: the most signals first, then the larger share
// dismissed or muted, then event type and label, no label first; a scope
// listed twice counts once; deliveries fewer than signals use the signals.
func TestFindPatternsOrder(t *testing.T) {
	windowStart := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	a := domain.FeedbackAnalytics{Since: windowStart.Add(time.Hour), Groups: []domain.FeedbackGroup{
		{EventType: "task.updated", Label: label("ops"), Delivered: 10, Dismissed: 6, DistinctUsers: 2},
		{EventType: "task.updated", Label: label("docs"), Delivered: 10, Dismissed: 6, DistinctUsers: 2},
		{EventType: "task.updated", Delivered: 10, Dismissed: 6, DistinctUsers: 2},
		{EventType: "task.created", Label: label("docs"), Delivered: 8, Dismissed: 6, DistinctUsers: 2},
		{EventType: "task.comment_added", Label: label("docs"), Delivered: 30, Dismissed: 20, Muted: 5, DistinctUsers: 5},
		{EventType: "task.updated", Label: label("docs"), Delivered: 99, Dismissed: 99, DistinctUsers: 9},
		{EventType: "task.due_soon", Label: label("x"), Delivered: 0, Dismissed: 5, Acted: 1, DistinctUsers: 2},
	}}
	got, err := FindPatterns(a, windowStart)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"task.comment_added, label docs: 25 signals, dismiss 0.667, mute 0.167",
		"task.created, label docs: 6 signals, dismiss 0.75, mute 0",
		"task.updated, no label: 6 signals, dismiss 0.6, mute 0",
		"task.updated, label docs: 6 signals, dismiss 0.6, mute 0",
		"task.updated, label ops: 6 signals, dismiss 0.6, mute 0",
		"task.due_soon, label x: 5 signals, dismiss 0.833, mute 0",
	}, "\n")
	if describePatterns(got) != want {
		t.Errorf("patterns:\n%s\nwant:\n%s", describePatterns(got), want)
	}
	// The same input gives the same answer.
	again, _ := FindPatterns(a, windowStart)
	if describePatterns(again) != describePatterns(got) {
		t.Error("FindPatterns is not deterministic")
	}
}
