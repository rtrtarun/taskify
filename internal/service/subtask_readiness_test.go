package service

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// TestSubtaskReadiness is M8-04: completing the last blocker of a subtask
// sends exactly one task.unblocked, through the M5-04 resolution logic.
func TestSubtaskReadiness(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		parent := f.create(t, nil)
		kids := byTitle(f.decompose(t, parent, uuid.New(), launchPlan))
		release := kids["Release"]

		f.setStatus(t, kids["Build"], domain.StatusCompleted)
		if got := f.get(t, release.ID); got.Status != domain.StatusBlocked {
			t.Fatalf("Release with Test still open is %s", got.Status)
		}
		if ev := f.eventTypes(t, release.ID, 0); len(ev) != 0 {
			t.Errorf("Release was told %v after its first blocker", ev)
		}
		// Build was Test's only blocker.
		if ev := f.eventTypes(t, kids["Test"].ID, 0); !slices.Equal(ev, []string{domain.EventTaskUnblocked}) {
			t.Errorf("Test events = %v", ev)
		}

		f.setStatus(t, kids["Test"], domain.StatusInProgress)
		f.setStatus(t, kids["Test"], domain.StatusCompleted)
		got := f.get(t, release.ID)
		if got.Status != domain.StatusTodo || got.BlockedReason != nil {
			t.Errorf("Release after both blockers is %s", got.Status)
		}
		ns := f.notifications(t, release.ID)
		if len(ns) != 1 || ns[0].EventType != domain.EventTaskUnblocked || ns[0].UserID != bob.ID ||
			ns[0].Payload["blockerTaskId"] != kids["Test"].ID.String() {
			t.Fatalf("Release notifications = %+v, want one task.unblocked naming Test", ns)
		}
	})
}
