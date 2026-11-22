package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// TestNotificationsViewTheApp is AP-10's wiring: with AppBaseURL set, the
// rows a task change writes view the task in the web app; without it they
// carry no view link.
func TestNotificationsViewTheApp(t *testing.T) {
	for _, app := range []string{"", "https://app.taskify.test"} {
		b := repotest.Memory()
		svc := NewTaskService(Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments, Outbox: b.Outbox,
			Dependencies: b.Deps, AgentJobs: b.AgentJobs},
			Config{PublicBaseURL: "https://taskify.test", AppBaseURL: app}, WithClock(func() time.Time { return t0 }))
		if _, err := svc.CreateWorkspace(ctx, tenant, "ws_core", "Core"); err != nil {
			t.Fatal(err)
		}
		task, err := svc.CreateTask(ctx, alice, domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "x", AssigneeID: bob.ID})
		if err != nil {
			t.Fatal(err)
		}
		entries, err := b.Outbox.ListForAggregate(ctx, task.ID)
		if err != nil || len(entries) != 1 {
			t.Fatalf("outbox = %v, %v", entries, err)
		}
		var n struct {
			Payload struct {
				ActionURLs map[string]string `json:"actionUrls"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(entries[0].Payload, &n); err != nil {
			t.Fatal(err)
		}
		view, ok := n.Payload.ActionURLs[domain.ActionView]
		switch {
		case app == "" && ok:
			t.Errorf("without the app: view = %q", view)
		case app != "" && view != app+"/tasks/"+task.ID.String():
			t.Errorf("with the app: actionUrls = %v", n.Payload.ActionURLs)
		}
	}
}
