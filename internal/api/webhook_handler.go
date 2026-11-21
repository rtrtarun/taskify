package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/service"
)

// Webhook headers (default G7; checked against AgentPulse in M0-07).
const (
	HeaderWebhookTimestamp = "X-AgentPulse-Timestamp"
	HeaderWebhookSignature = "X-AgentPulse-Signature"
	// ReplayWindow bounds how far a callback's timestamp may be from now.
	ReplayWindow = 5 * time.Minute
)

// SignWebhook returns the signature AgentPulse sends for a body: hex
// HMAC-SHA256 over "<timestamp>.<body>". Exported for tests and tools.
func SignWebhook(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// webhookBody is AgentPulse's callback. Its fields are camelCase, like the
// rest of AgentPulse's API. Unknown fields are ignored so AgentPulse can add
// some without breaking Taskify.
type webhookBody struct {
	Action         string `json:"action"`
	TenantID       string `json:"tenantId"`
	UserID         string `json:"userId"`
	TaskID         string `json:"taskId"`
	JobID          string `json:"jobId"` // approve and reject
	NotificationID string `json:"notificationId"`
	Duration       string `json:"duration"`
}

// webhook authenticates by signature, not by JWT: the caller is AgentPulse,
// acting for the user named in the body.
func (h *handlers) webhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		h.fail(w, r, newError(http.StatusRequestEntityTooLarge, CodeInvalidRequest, "request body is larger than 1 MiB"))
		return
	}
	sig, err := h.verifyWebhook(r, raw)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	var body webhookBody
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&body); err != nil {
		h.fail(w, r, badRequest("callback body is not valid JSON"))
		return
	}
	cb := service.Callback{Key: sig, Action: body.Action, TenantID: body.TenantID, UserID: body.UserID,
		NotificationID: body.NotificationID, Duration: body.Duration}
	// A malformed ID names nothing; an absent one is for the service to judge.
	for _, f := range []struct {
		raw string
		dst *uuid.UUID
	}{{body.TaskID, &cb.TaskID}, {body.JobID, &cb.JobID}} {
		if f.raw == "" {
			continue
		}
		id, err := uuid.Parse(f.raw)
		if err != nil {
			h.fail(w, r, domain.ErrNotFound)
			return
		}
		*f.dst = id
	}
	dup, err := h.webhooks.Handle(r.Context(), cb)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := "applied"
	if dup {
		status = "duplicate"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// verifyWebhook checks the timestamp window and the signature, comparing in
// constant time, and returns the signature for replay detection.
func (h *handlers) verifyWebhook(r *http.Request, body []byte) (string, error) {
	unauthorized := func(msg string) error { return newError(http.StatusUnauthorized, CodeUnauthorized, msg) }

	ts, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get(HeaderWebhookTimestamp)), 10, 64)
	if err != nil {
		return "", unauthorized("missing or malformed " + HeaderWebhookTimestamp)
	}
	age := h.now().Sub(time.Unix(ts, 0))
	if age > ReplayWindow || age < -ReplayWindow {
		return "", unauthorized("callback timestamp is outside the 5-minute window")
	}
	got := strings.TrimPrefix(strings.TrimSpace(r.Header.Get(HeaderWebhookSignature)), "sha256=")
	gotBytes, err := hex.DecodeString(got)
	if err != nil || len(gotBytes) != sha256.Size {
		return "", unauthorized("missing or malformed " + HeaderWebhookSignature)
	}
	want, _ := hex.DecodeString(SignWebhook(h.webhookSecret, ts, body))
	if !hmac.Equal(gotBytes, want) {
		return "", unauthorized("signature does not match")
	}
	return strings.ToLower(got), nil
}
