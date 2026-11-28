package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/service"
)

// feedRetryAfter is the hint sent with a 503 from the feed, in seconds. It is
// the circuit breaker's cooldown, the soonest a retry can be tried.
const feedRetryAfter = 10

func (h *handlers) notificationRoutes(r chi.Router) {
	r.Get("/in-app", h.listInAppNotifications)
	r.Post("/{id}/ack", h.ackNotification)
}

type notificationTaskJSON struct {
	ID         uuid.UUID `json:"id"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	Priority   string    `json:"priority"`
	AssigneeID *string   `json:"assignee_id"`
	Labels     []string  `json:"labels"`
}

type inAppNotificationJSON struct {
	ID           string                `json:"id"`
	EventType    string                `json:"event_type"`
	Title        string                `json:"title"`
	Body         string                `json:"body"`
	CreatedAt    string                `json:"created_at"`
	TaskID       *uuid.UUID            `json:"task_id"`
	JobID        *uuid.UUID            `json:"job_id"`
	Task         *notificationTaskJSON `json:"task"`
	Read         bool                  `json:"read"`
	Signal       *string               `json:"signal"`
	SnoozedUntil *string               `json:"snoozed_until"`
}

type notificationFeedJSON struct {
	Notifications []inAppNotificationJSON `json:"notifications"`
	NextCursor    *string                 `json:"next_cursor"`
}

// listInAppNotifications serves GET /notifications/in-app.
//
// The tenant and user are the caller's, from the verified token. The only
// request inputs are limit and cursor: there is no parameter, header, or body
// field that names a person, so none can redirect the call to another feed.
func (h *handlers) listInAppNotifications(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > service.MaxFeedLimit {
			h.fail(w, r, badRequest("limit must be an integer from 1 to 100"))
			return
		}
		limit = n
	}
	id := identity(r)
	page, err := h.notifications.Feed(r.Context(), id.TenantID, id.UserID, limit, q.Get("cursor"))
	switch {
	case errors.Is(err, service.ErrFeedUnavailable):
		// The rest of the API is unaffected; say when to come back.
		h.logger.WarnContext(r.Context(), "in-app feed unavailable", "error", err)
		w.Header().Set("Retry-After", strconv.Itoa(feedRetryAfter))
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: errorDetail{
			Code:    CodeUpstreamUnavailable,
			Message: "notifications are unavailable right now; try again shortly",
			Details: map[string]any{"retry_after_seconds": feedRetryAfter},
		}})
		return
	case errors.Is(err, domain.ErrValidation):
		h.fail(w, r, badRequest(err.Error()))
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	out := notificationFeedJSON{Notifications: make([]inAppNotificationJSON, 0, len(page.Items))}
	if page.Next != "" {
		out.NextCursor = &page.Next
	}
	for _, it := range page.Items {
		n := inAppNotificationJSON{ID: it.ID, EventType: it.EventType, Title: it.Title, Body: it.Body,
			CreatedAt: rfc3339(it.CreatedAt), TaskID: it.TaskID, JobID: it.JobID}
		if t := it.Task; t != nil {
			var assignee *string
			if t.AssigneeID != "" {
				a := t.AssigneeID
				assignee = &a
			}
			labels := t.Labels
			if labels == nil {
				labels = []string{}
			}
			n.Task = &notificationTaskJSON{ID: t.ID, Title: t.Title, Status: string(t.Status), Priority: string(t.Priority),
				AssigneeID: assignee, Labels: labels}
		}
		if a := it.Ack; a != nil {
			n.Read = true
			sig := a.Signal
			n.Signal = &sig
			if a.SnoozeUntil != nil {
				u := rfc3339(*a.SnoozeUntil)
				n.SnoozedUntil = &u
			}
		}
		out.Notifications = append(out.Notifications, n)
	}
	writeJSON(w, http.StatusOK, out)
}

type ackRequest struct {
	Signal   string `json:"signal"`
	Duration string `json:"duration"`
}

type ackJSON struct {
	NotificationID string  `json:"notification_id"`
	Signal         string  `json:"signal"`
	SnoozedUntil   *string `json:"snoozed_until"`
	AckedAt        string  `json:"acked_at"`
}

// ackNotification serves POST /notifications/{id}/ack. The ack is the
// caller's own: it is keyed by the token's tenant and user.
func (h *handlers) ackNotification(w http.ResponseWriter, r *http.Request) {
	var req ackRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	id := identity(r)
	ack, err := h.notifications.Ack(r.Context(), service.AckInput{
		TenantID: id.TenantID, UserID: id.UserID, NotificationID: chi.URLParam(r, "id"),
		Signal: req.Signal, Duration: req.Duration,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ackJSON{NotificationID: ack.NotificationID, Signal: ack.Signal, AckedAt: rfc3339(ack.AckedAt)}
	if ack.SnoozeUntil != nil {
		u := rfc3339(*ack.SnoozeUntil)
		out.SnoozedUntil = &u
	}
	writeJSON(w, http.StatusOK, out)
}
