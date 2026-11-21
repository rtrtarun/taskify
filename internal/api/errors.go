// Package api is Taskify's HTTP interface: routing, authentication,
// request decoding, and the mapping from domain errors to HTTP responses.
// Handlers stay thin; rules live in the service and domain packages.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/service"
)

// Error codes in the response body. Clients branch on these, not on the
// message text.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeValidation          = "validation_failed"
	CodeInvalidTransition   = "invalid_transition"
	CodeUnauthorized        = "unauthorized"
	CodeForbidden           = "forbidden"
	CodeNotFound            = "not_found"
	CodeVersionConflict     = "version_conflict"
	CodeAlreadyExists       = "already_exists"
	CodeVersionRequired     = "version_required"
	CodeRateLimited         = "rate_limited"
	CodeUpstreamError       = "upstream_error"
	CodeUpstreamUnavailable = "upstream_unavailable"
	CodeInternal            = "internal_error"
	CodeAgentDisabled       = "agent_disabled"
)

// httpError is an error a handler raises with a specific status and code.
type httpError struct {
	status  int
	code    string
	message string
	details map[string]any
}

func (e *httpError) Error() string { return e.message }

func newError(status int, code, message string) *httpError {
	return &httpError{status: status, code: code, message: message}
}

func badRequest(message string) *httpError {
	return newError(http.StatusBadRequest, CodeInvalidRequest, message)
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// writeError maps err to a response, following docs/architecture.md section
// 5.4 and default G4: unparseable input is 400, a rule violation is 422.
func writeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	e := classify(err)
	if e.status >= 500 {
		logger.ErrorContext(r.Context(), "request failed", "status", e.status, "error", err)
	}
	writeJSON(w, e.status, errorBody{Error: errorDetail{Code: e.code, Message: e.message, Details: e.details}})
}

func classify(err error) *httpError {
	var he *httpError
	var conflict *service.VersionConflictError
	var se *agentpulse.StatusError
	switch {
	case errors.As(err, &he):
		return he
	case errors.As(err, &conflict):
		return &httpError{status: http.StatusConflict, code: CodeVersionConflict, message: conflict.Error(),
			details: map[string]any{"current_version": conflict.Current}}
	case errors.Is(err, domain.ErrVersionConflict):
		return newError(http.StatusConflict, CodeVersionConflict, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return newError(http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, domain.ErrForbidden):
		return newError(http.StatusForbidden, CodeForbidden, err.Error())
	case errors.Is(err, service.ErrStaleProposal):
		return newError(http.StatusConflict, CodeStaleVersion, err.Error())
	case errors.Is(err, service.ErrAlreadyDecided):
		return newError(http.StatusConflict, CodeAlreadyDecided, err.Error())
	case errors.Is(err, service.ErrNotAwaitingApproval):
		return newError(http.StatusConflict, CodeNotAwaitingApproval, err.Error())
	case errors.Is(err, service.ErrAgentDisabled):
		return newError(http.StatusConflict, CodeAgentDisabled, err.Error())
	case errors.Is(err, domain.ErrAlreadyExists):
		return newError(http.StatusConflict, CodeAlreadyExists, err.Error())
	case errors.Is(err, domain.ErrInvalidStateTransition):
		return newError(http.StatusUnprocessableEntity, CodeInvalidTransition, err.Error())
	case errors.Is(err, domain.ErrValidation):
		return newError(http.StatusUnprocessableEntity, CodeValidation, err.Error())
	case errors.Is(err, agentpulse.ErrCircuitOpen):
		return newError(http.StatusServiceUnavailable, CodeUpstreamUnavailable, "AgentPulse is unavailable; try again shortly")
	case errors.As(err, &se) && se.Code == http.StatusNotFound:
		return newError(http.StatusNotFound, CodeNotFound, "not found in AgentPulse")
	case errors.As(err, &se) && se.Code == http.StatusConflict:
		return newError(http.StatusConflict, CodeAlreadyExists, "AgentPulse reported a conflict")
	case agentpulse.IsUpstream(err):
		return newError(http.StatusBadGateway, CodeUpstreamError, "AgentPulse did not complete the request")
	}
	// The message of an unexpected error may describe internals; it is
	// logged, not returned.
	return newError(http.StatusInternalServerError, CodeInternal, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxBody bounds every request body.
const maxBody = 1 << 20

// decodeJSON reads one JSON object into v, rejecting unknown fields so a
// mistyped field name is an error rather than silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return newError(http.StatusRequestEntityTooLarge, CodeInvalidRequest, "request body is larger than 1 MiB")
		}
		return badRequest("request body is not valid: " + err.Error())
	}
	if dec.More() {
		return badRequest("request body must hold a single JSON object")
	}
	return nil
}

// decodeOptionalJSON is decodeJSON for endpoints where the body may be
// omitted: an empty body leaves v unchanged.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return newError(http.StatusRequestEntityTooLarge, CodeInvalidRequest, "request body is larger than 1 MiB")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return decodeJSON(w, r, v)
}
