package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/RONGALI-TARUN/taskify/internal/domain"
	"github.com/RONGALI-TARUN/taskify/internal/repository"
	"github.com/RONGALI-TARUN/taskify/internal/service"
)

func (h *handlers) taskRoutes(r chi.Router) {
	r.Post("/", h.createTask)
	r.Get("/", h.listTasks)
	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", h.getTask)
		r.Put("/", h.updateTask)
		r.Patch("/", h.updateTask)
		r.Delete("/", h.deleteTask)
		r.Post("/complete", h.completeTask)
		r.Post("/restore", h.restoreTask)
		r.Post("/comments", h.addComment)
		r.Post("/feedback", h.recordFeedback)
	})
}

func taskID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		// A malformed ID cannot name a task: answer as for any unknown task.
		return uuid.Nil, domain.ErrNotFound
	}
	return id, nil
}

func identity(r *http.Request) Identity {
	id, _ := IdentityFrom(r.Context())
	return id
}

func (h *handlers) writeTask(w http.ResponseWriter, status int, t *domain.Task) {
	w.Header().Set("ETag", etag(t.Version))
	writeJSON(w, status, toTaskJSON(t))
}

func (h *handlers) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	id := identity(r)
	task, err := h.tasks.CreateTask(r.Context(), domain.User(id.UserID), domain.NewTaskInput{
		TenantID:    id.TenantID,
		WorkspaceID: req.WorkspaceID,
		Title:       req.Title,
		Description: req.Description,
		Priority:    domain.Priority(req.Priority),
		AssigneeID:  req.AssigneeID,
		Labels:      req.Labels,
		Metadata:    req.Metadata,
		Watchers:    req.WatcherIDs,
		DueAt:       req.DueAt,
		RemindAt:    req.RemindAt,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/tasks/"+task.ID.String())
	h.writeTask(w, http.StatusCreated, task)
}

func (h *handlers) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := repository.TaskFilter{
		TenantID:   identity(r).TenantID,
		AssigneeID: q.Get("assignee_id"),
		Status:     domain.Status(q.Get("status")),
		Priority:   domain.Priority(q.Get("priority")),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > service.MaxListLimit {
			h.fail(w, r, badRequest("limit must be an integer from 1 to 200"))
			return
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		c, err := decodeCursor(v)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		f.After = c
	}
	page, err := h.tasks.ListTasks(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := taskListJSON{Tasks: make([]taskJSON, 0, len(page.Tasks)), NextCursor: encodeCursor(page.Next)}
	for _, t := range page.Tasks {
		out.Tasks = append(out.Tasks, toTaskJSON(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) getTask(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d, err := h.tasks.GetTask(r.Context(), identity(r).TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := taskDetailJSON{taskJSON: toTaskJSON(d.Task), Comments: make([]commentJSON, 0, len(d.Comments))}
	for _, c := range d.Comments {
		out.Comments = append(out.Comments, toCommentJSON(c))
	}
	w.Header().Set("ETag", etag(d.Task.Version))
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) updateTask(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req updateTaskRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	version, err := requiredVersion(r, req.Version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	patch, err := req.patch()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	who := identity(r)
	task, err := h.tasks.UpdateTask(r.Context(), domain.User(who.UserID), who.TenantID, id, version, patch)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeTask(w, http.StatusOK, task)
}

// requiredVersion takes the expected version from If-Match or the body.
func requiredVersion(r *http.Request, body optional[int]) (int, error) {
	header := r.Header.Get("If-Match")
	switch {
	case header != "":
		v, err := parseIfMatch(header)
		if err != nil {
			return 0, err
		}
		if body.Set && !body.Null && body.Value != v {
			return 0, badRequest("If-Match and version disagree")
		}
		return v, nil
	case body.Set && !body.Null && body.Value >= 1:
		return body.Value, nil
	case body.Set:
		return 0, badRequest("version must be a positive integer")
	}
	return 0, newError(http.StatusPreconditionRequired, CodeVersionRequired,
		"send the task version in If-Match or in the body, so a concurrent edit is not overwritten")
}

// optionalVersion reads If-Match if present, for actions where a version
// check is offered but not required.
func optionalVersion(r *http.Request) (*int, error) {
	h := r.Header.Get("If-Match")
	if h == "" {
		return nil, nil
	}
	v, err := parseIfMatch(h)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

type taskAction func(r *http.Request, who Identity, id uuid.UUID, version *int) (*domain.Task, error)

// runAction handles the POST and DELETE actions that take an optional
// If-Match and return the task.
func (h *handlers) runAction(w http.ResponseWriter, r *http.Request, act taskAction) {
	id, err := taskID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	version, err := optionalVersion(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	task, err := act(r, identity(r), id, version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeTask(w, http.StatusOK, task)
}

type completeRequest struct {
	// NotificationID, when the completion came from a notification, is
	// forwarded to AgentPulse as acted feedback.
	NotificationID string `json:"notification_id"`
}

func (h *handlers) completeTask(w http.ResponseWriter, r *http.Request) {
	var req completeRequest
	if err := decodeOptionalJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	h.runAction(w, r, func(r *http.Request, who Identity, id uuid.UUID, v *int) (*domain.Task, error) {
		task, err := h.tasks.CompleteTask(r.Context(), domain.User(who.UserID), who.TenantID, id, v)
		if err == nil && req.NotificationID != "" {
			// The task is completed either way; feedback is best effort.
			fbErr := h.feedback.Record(r.Context(), service.FeedbackInput{
				TenantID: who.TenantID, UserID: who.UserID, TaskID: id,
				NotificationID: req.NotificationID, Signal: service.SignalActed,
			})
			if fbErr != nil {
				h.logger.WarnContext(r.Context(), "completed, but acted feedback was not forwarded", "task_id", id, "error", fbErr)
			}
		}
		return task, err
	})
}

func (h *handlers) restoreTask(w http.ResponseWriter, r *http.Request) {
	h.runAction(w, r, func(r *http.Request, who Identity, id uuid.UUID, v *int) (*domain.Task, error) {
		return h.tasks.RestoreTask(r.Context(), domain.User(who.UserID), who.TenantID, id, v)
	})
}

// deleteTask archives a task. With ?permanent=true it removes it for good,
// which only admins may do.
func (h *handlers) deleteTask(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("permanent") {
	case "", "false":
		h.runAction(w, r, func(r *http.Request, who Identity, id uuid.UUID, v *int) (*domain.Task, error) {
			return h.tasks.ArchiveTask(r.Context(), domain.User(who.UserID), who.TenantID, id, v)
		})
	case "true":
		if err := requireAdmin(r); err != nil {
			h.fail(w, r, err)
			return
		}
		id, err := taskID(r)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if err := h.tasks.DeleteTask(r.Context(), identity(r).TenantID, id); err != nil {
			h.fail(w, r, err)
			return
		}
		h.logger.InfoContext(r.Context(), "task permanently deleted", "task_id", id, "by", identity(r).UserID)
		w.WriteHeader(http.StatusNoContent)
	default:
		h.fail(w, r, badRequest("permanent must be true or false"))
	}
}

type commentRequest struct {
	Body string `json:"body"`
}

func (h *handlers) addComment(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req commentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	who := identity(r)
	c, err := h.tasks.AddComment(r.Context(), domain.User(who.UserID), who.TenantID, id, req.Body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toCommentJSON(c))
}

type feedbackRequest struct {
	Signal         string `json:"signal"`
	NotificationID string `json:"notification_id"`
	Duration       string `json:"duration"`
}

// recordFeedback forwards a reaction to AgentPulse. The signal and duration may
// come from the query string, as in the actionUrls Taskify puts in
// notifications, or from a JSON body, which takes precedence.
func (h *handlers) recordFeedback(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	req := feedbackRequest{Signal: q.Get("signal"), Duration: q.Get("duration"), NotificationID: q.Get("notification_id")}
	var body feedbackRequest
	if err := decodeOptionalJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	if body.Signal != "" {
		req.Signal = body.Signal
	}
	if body.Duration != "" {
		req.Duration = body.Duration
	}
	if body.NotificationID != "" {
		req.NotificationID = body.NotificationID
	}
	who := identity(r)
	err = h.feedback.Record(r.Context(), service.FeedbackInput{
		TenantID: who.TenantID, UserID: who.UserID, TaskID: id,
		NotificationID: req.NotificationID, Signal: req.Signal, Duration: req.Duration,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "forwarded"})
}
