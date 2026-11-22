package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// Links are the base URLs a notification's links are built from (AP-10).
type Links struct {
	// API is Taskify's externally reachable address (PUBLIC_BASE_URL). The
	// action URLs name its endpoints.
	API string
	// App is the web app's address (APP_BASE_URL), or empty when it is not
	// set. With it, every notification about a task carries
	// actionUrls.view {App}/tasks/{id}, and the morning brief's view opens
	// {App}/brief. Without it, task notifications carry no view link, and
	// approvals, escalations, and the brief keep their links to the API.
	App string
}

// TaskView is the web app's page for a task, or "" when App is not set.
func (l Links) TaskView(id uuid.UUID) string {
	if l.App == "" {
		return ""
	}
	return fmt.Sprintf("%s/tasks/%s", l.App, id)
}

// taskAPI is the API URL of a task.
func (l Links) taskAPI(id uuid.UUID) string {
	return fmt.Sprintf("%s/api/v1/tasks/%s", l.API, id)
}

// jobAPI is the API URL of an agent job.
func (l Links) jobAPI(id uuid.UUID) string {
	return fmt.Sprintf("%s/api/v1/agent-jobs/%s", l.API, id)
}

// taskOrAPI is the app's page for a task, or its API URL without the app:
// where a person following a link about the task is sent.
func (l Links) taskOrAPI(id uuid.UUID) string {
	if v := l.TaskView(id); v != "" {
		return v
	}
	return l.taskAPI(id)
}

// BriefView is where the morning brief's view link goes: the app's brief
// page, or, without the app, the brief endpoint (GET /api/v1/me/brief).
func (l Links) BriefView() string {
	if l.App != "" {
		return l.App + "/brief"
	}
	return l.API + "/api/v1/me/brief"
}
