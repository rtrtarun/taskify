package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/pkg/client"
)

// working reports whether a job may still change by itself. awaiting_approval
// is not working: it waits for a person.
func working(s client.AgentJobStatus) bool {
	return s == client.AgentJobStatusPending || s == client.AgentJobStatusRunning
}

// followJob asks for the job until it stops working, calling onChange with
// each new status (the first included). timeout 0 means no limit besides
// Ctrl-C. It returns the last job and its body.
func (a *app) followJob(ctx context.Context, s *session, c *client.ClientWithResponses, id uuid.UUID, timeout time.Duration,
	onChange func(body []byte, j *client.AgentJob) error) (*client.AgentJob, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var last client.AgentJobStatus
	for {
		resp, err := c.GetAgentJobWithResponse(ctx, id)
		if err != nil && ctx.Err() != nil {
			return nil, nil, a.followStopped(ctx, timeout)
		}
		if err := s.check(resp, err, http.StatusOK); err != nil {
			return nil, nil, err
		}
		j := resp.JSON200
		if j.Status != last {
			last = j.Status
			if err := onChange(resp.Body, j); err != nil {
				return nil, nil, err
			}
		}
		if !working(j.Status) {
			return j, resp.Body, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, a.followStopped(ctx, timeout)
		case <-time.After(a.pollEvery):
		}
	}
}

func (a *app) followStopped(ctx context.Context, timeout time.Duration) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fail(exitUnexpected, "the job was still running after %s; it carries on, follow it with: taskctl job show --watch", timeout)
	}
	return fail(exitUnexpected, "interrupted")
}

// finished turns a job that stopped into the command's outcome: a failed job
// is exit 1, with its error.
func finished(j *client.AgentJob) error {
	if j.Status == client.AgentJobStatusFailed {
		msg := "no error recorded"
		if j.ErrorMessage != nil {
			msg = *j.ErrorMessage
		}
		return fail(exitUnexpected, "job %s failed: %s", j.Id, msg)
	}
	return nil
}

func progress(a *app, format string, args ...any) {
	if !a.jsonOut && !a.quiet {
		_, _ = fmt.Fprintf(a.stderr, "taskctl: "+format+"\n", args...)
	}
}
