package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/rtrtarun/taskify/pkg/client"
)

func parseJobID(arg string) (uuid.UUID, error) {
	id, err := uuid.Parse(arg)
	if err != nil || !fullUUID.MatchString(arg) {
		return uuid.Nil, fail(exitUsage, "%q is not a job ID (the full ID, as 'taskctl job list' shows it)", arg)
	}
	return id, nil
}

func (a *app) newJobCmd() *cobra.Command {
	cmd := group(&cobra.Command{Use: "job", Short: "Agent jobs and their proposals"})
	cmd.AddCommand(a.jobList(), a.jobShow(), a.jobDecide("approve", "Approve a job's proposals and apply them"),
		a.jobDecide("reject", "Reject a job's proposals"))
	return cmd
}

func jobRow(j client.AgentJob) []string {
	task := "-"
	if j.TaskId != nil {
		task = shortID(j.TaskId.String())
	}
	return []string{j.Id.String(), string(j.AgentType), string(j.Status), task, fmtTime(j.CreatedAt)}
}

func (a *app) jobList() *cobra.Command {
	var status, task string
	var limit int
	cmd := &cobra.Command{Use: "list", Short: "List agent jobs, newest first", Args: cobra.NoArgs}
	f := cmd.Flags()
	f.StringVar(&status, "status", "", "pending, running, awaiting_approval, completed, failed, or dismissed")
	f.StringVar(&task, "task", "", "only jobs about this task (ID or prefix)")
	f.IntVar(&limit, "limit", 50, "how many jobs, 1 to 200")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, _ []string) error {
		p := &client.ListAgentJobsParams{}
		if f.Changed("limit") {
			p.Limit = &limit
		}
		if status != "" {
			st := client.AgentJobStatus(status)
			p.Status = &st
		}
		if task != "" {
			id, err := s.resolveTaskID(ctx, c, task)
			if err != nil {
				return err
			}
			p.TaskId = &id
		}
		resp, err := c.ListAgentJobsWithResponse(ctx, p)
		if err := s.check(resp, err, http.StatusOK); err != nil {
			return err
		}
		jobs := resp.JSON200.AgentJobs
		ids := make([]string, len(jobs))
		rows := make([][]string, len(jobs))
		for i, j := range jobs {
			ids[i], rows[i] = j.Id.String(), jobRow(j)
		}
		if err := a.print(result{raw: resp.Body, ids: ids, table: func(w io.Writer) {
			tabular(w, []string{"ID", "AGENT", "STATUS", "TASK", "CREATED"}, rows)
		}}); err != nil {
			return err
		}
		if resp.JSON200.NextCursor != nil {
			_, _ = fmt.Fprintln(a.stderr, "taskctl: more jobs exist; raise --limit to see them")
		}
		return nil
	})
	return cmd
}

func jobCard(w io.Writer, j client.AgentJob) {
	f := func(k, v string) { _, _ = fmt.Fprintf(w, "%-10s %s\n", k+":", v) }
	f("id", j.Id.String())
	f("agent", string(j.AgentType))
	f("status", string(j.Status))
	if j.TaskId != nil {
		f("task", j.TaskId.String())
	}
	f("attempts", fmt.Sprint(j.Attempts))
	f("created", fmtTime(j.CreatedAt))
	if j.DecidedBy != nil {
		f("decided", *j.DecidedBy)
	}
	if j.ErrorMessage != nil {
		f("error", *j.ErrorMessage)
	}
	if j.ProposedActions != nil && len(*j.ProposedActions) > 0 {
		_, _ = fmt.Fprintln(w, "proposed:")
		for _, p := range *j.ProposedActions {
			args, _ := json.Marshal(p.Arguments)
			_, _ = fmt.Fprintf(w, "  %s %s\n", p.Tool, strings.TrimSpace(string(args)))
		}
	}
}

func (a *app) jobShow() *cobra.Command {
	var watch bool
	var timeout time.Duration
	cmd := &cobra.Command{Use: "show <id>", Short: "Show a job and what it proposes", Args: cobra.ExactArgs(1)}
	cmd.Flags().BoolVar(&watch, "watch", false, "follow the job until it finishes or waits for approval, printing each change")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "with --watch, give up after this long (default: no limit)")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
		id, err := parseJobID(args[0])
		if err != nil {
			return err
		}
		show := func(body []byte, j *client.AgentJob) error {
			return a.print(result{raw: body, ids: []string{j.Id.String()}, table: func(w io.Writer) { jobCard(w, *j) }})
		}
		if !watch {
			resp, err := c.GetAgentJobWithResponse(ctx, id)
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			return show(resp.Body, resp.JSON200)
		}
		j, _, err := a.followJob(ctx, s, c, id, timeout, func(body []byte, j *client.AgentJob) error {
			if !a.jsonOut && !a.quiet {
				_, _ = fmt.Fprintf(a.stdout, "-- %s --\n", j.Status)
			}
			return show(body, j)
		})
		if err != nil {
			return err
		}
		return finished(j)
	})
	return cmd
}

// jobDecide is approve (the server's apply) or reject (its dismiss). Whether
// the proposal is still current, and who may decide, are the server's calls:
// a stale proposal is a 409 (exit 5), a stranger a 403 (exit 3).
func (a *app) jobDecide(verb, short string) *cobra.Command {
	return &cobra.Command{Use: verb + " <id>", Short: short, Args: cobra.ExactArgs(1),
		RunE: a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
			id, err := parseJobID(args[0])
			if err != nil {
				return err
			}
			var body []byte
			var d *client.AgentJobDecision
			if verb == "approve" {
				resp, err := c.ApplyAgentJobWithResponse(ctx, id)
				if err := s.check(resp, err, http.StatusOK); err != nil {
					return err
				}
				body, d = resp.Body, resp.JSON200
			} else {
				resp, err := c.DismissAgentJobWithResponse(ctx, id)
				if err := s.check(resp, err, http.StatusOK); err != nil {
					return err
				}
				body, d = resp.Body, resp.JSON200
			}
			return a.print(result{raw: body, ids: []string{d.Job.Id.String()}, table: func(w io.Writer) {
				note := ""
				if !d.Changed {
					note = " (already decided that way; nothing changed)"
				}
				_, _ = fmt.Fprintf(w, "job %s is now %s%s\n", d.Job.Id, d.Job.Status, note)
			}})
		})}
}
