package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/rtrtarun/taskify/pkg/client"
)

// Commands that call the API share this: build the session and client, run,
// and report any plain error as an unexpected failure (exit 1).
type apiFunc func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error

func (a *app) api(fn apiFunc) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		s, err := a.session()
		if err != nil {
			return err
		}
		c, err := a.newClient(s)
		if err != nil {
			return err
		}
		if err := fn(cmd.Context(), s, c, args); err != nil {
			return failure(err)
		}
		return nil
	}
}

var (
	fullUUID   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	idPrefixRE = regexp.MustCompile(`^[0-9a-fA-F-]{1,36}$`)
)

// resolveTaskID turns a full ID or a unique prefix into the task's ID. The
// server does the matching (the id_prefix filter); the CLI only decides
// whether the answer is one task.
func (s *session) resolveTaskID(ctx context.Context, c *client.ClientWithResponses, arg string) (uuid.UUID, error) {
	if fullUUID.MatchString(arg) {
		return uuid.MustParse(strings.ToLower(arg)), nil
	}
	if !idPrefixRE.MatchString(arg) {
		return uuid.Nil, fail(exitUsage, "%q is not a task ID or an ID prefix (hexadecimal and hyphens, at most 36 characters)", arg)
	}
	find := func(status *client.TaskStatus) ([]client.Task, error) {
		limit := 5
		resp, err := c.ListTasksWithResponse(ctx, &client.ListTasksParams{IdPrefix: &arg, Status: status, Limit: &limit})
		if err := s.check(resp, err, http.StatusOK); err != nil {
			return nil, err
		}
		return resp.JSON200.Tasks, nil
	}
	tasks, err := find(nil)
	if err == nil && len(tasks) == 0 {
		// Archived tasks are listed only when asked for.
		archived := client.Archived
		tasks, err = find(&archived)
	}
	if err != nil {
		return uuid.Nil, err
	}
	switch len(tasks) {
	case 0:
		return uuid.Nil, fail(exitNotFound, "no task has an ID starting with %q", arg)
	case 1:
		return tasks[0].Id, nil
	}
	var ids []string
	for _, t := range tasks {
		ids = append(ids, t.Id.String())
	}
	return uuid.Nil, fail(exitUsage, "%q matches more than one task (%s); use more characters", arg, strings.Join(ids, ", "))
}

func (a *app) newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Create, view, and change tasks"}
	cmd.AddCommand(a.taskAdd(), a.taskList(), a.taskShow(), a.taskUpdate(), a.taskDone(),
		a.taskBlock(), a.taskUnblock(), a.taskArchive(), a.taskComment(), a.taskDecompose())
	return cmd
}

// parseTime accepts an RFC 3339 time, or a date (midnight UTC), and returns
// it in UTC.
func parseTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	return time.Time{}, fail(exitUsage, "%q is not a time: use 2026-12-31 or 2026-12-31T17:00:00Z", v)
}

func (a *app) taskAdd() *cobra.Command {
	var desc, priority, assignee, due, workspace string
	var labels []string
	cmd := &cobra.Command{
		Use:   "add <title>",
		Short: "Create a task",
		Args:  cobra.ExactArgs(1),
	}
	f := cmd.Flags()
	f.StringVar(&desc, "desc", "", "description")
	f.StringVar(&priority, "priority", "", "p1_critical, p2_high, p3_medium, or p4_low")
	f.StringVar(&assignee, "assignee", "", "assignee user ID")
	f.StringVar(&due, "due", "", "due date, 2026-12-31 or an RFC 3339 time")
	f.StringSliceVar(&labels, "label", nil, "label (repeatable)")
	f.StringVar(&workspace, "workspace", "", "workspace ID (default: the profile's workspace)")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
		if workspace == "" {
			workspace = s.workspace
		}
		if workspace == "" {
			return fail(exitUsage, "no workspace: use --workspace or: taskctl config set workspace ID")
		}
		body := client.TaskCreate{Title: args[0], WorkspaceId: workspace}
		if f.Changed("desc") {
			body.Description = &desc
		}
		if f.Changed("priority") {
			p := client.Priority(priority)
			body.Priority = &p
		}
		if f.Changed("assignee") {
			body.AssigneeId = &assignee
		}
		if f.Changed("label") {
			body.Labels = &labels
		}
		if f.Changed("due") {
			t, err := parseTime(due)
			if err != nil {
				return err
			}
			body.DueAt = &t
		}
		resp, err := c.CreateTaskWithResponse(ctx, body)
		if err := s.check(resp, err, http.StatusCreated); err != nil {
			return err
		}
		return a.printTask(resp.Body, resp.JSON201)
	})
	return cmd
}

func (a *app) printTask(raw []byte, t *client.Task) error {
	return a.print(result{raw: raw, ids: []string{t.Id.String()}, table: func(w io.Writer) { taskCard(w, *t) }})
}

func (a *app) taskList() *cobra.Command {
	var status, priority, assignee string
	var mine bool
	var limit int
	cmd := &cobra.Command{Use: "list", Short: "List tasks, most recently updated first", Args: cobra.NoArgs}
	f := cmd.Flags()
	f.StringVar(&status, "status", "", "todo, in_progress, blocked, completed, or archived")
	f.StringVar(&priority, "priority", "", "p1_critical, p2_high, p3_medium, or p4_low")
	f.StringVar(&assignee, "assignee", "", "assignee user ID")
	f.BoolVar(&mine, "mine", false, "only tasks assigned to me")
	f.IntVar(&limit, "limit", 50, "how many tasks, 1 to 200")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, _ []string) error {
		p := &client.ListTasksParams{}
		if f.Changed("limit") {
			p.Limit = &limit
		}
		if status != "" {
			st := client.TaskStatus(status)
			p.Status = &st
		}
		if priority != "" {
			pr := client.Priority(priority)
			p.Priority = &pr
		}
		if mine && assignee != "" {
			return fail(exitUsage, "--mine and --assignee cannot be used together")
		}
		if mine {
			me, err := c.GetMeWithResponse(ctx)
			if err := s.check(me, err, http.StatusOK); err != nil {
				return err
			}
			assignee = me.JSON200.UserId
		}
		if assignee != "" {
			p.AssigneeId = &assignee
		}
		resp, err := c.ListTasksWithResponse(ctx, p)
		if err := s.check(resp, err, http.StatusOK); err != nil {
			return err
		}
		l := resp.JSON200
		ids := make([]string, len(l.Tasks))
		for i, t := range l.Tasks {
			ids[i] = t.Id.String()
		}
		if err := a.print(result{raw: resp.Body, ids: ids, table: func(w io.Writer) { taskTable(w, l.Tasks) }}); err != nil {
			return err
		}
		if l.NextCursor != nil {
			_, _ = fmt.Fprintln(a.stderr, "taskctl: more tasks exist; raise --limit to see them")
		}
		return nil
	})
	return cmd
}

func (a *app) taskShow() *cobra.Command {
	var comments, subtasks, runs bool
	cmd := &cobra.Command{Use: "show <id>", Short: "Show a task", Args: cobra.ExactArgs(1)}
	f := cmd.Flags()
	f.BoolVar(&comments, "comments", false, "include the recent comments")
	f.BoolVar(&subtasks, "subtasks", false, "include the subtasks")
	f.BoolVar(&runs, "agent-runs", false, "include agent activity on the task")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
		id, err := s.resolveTaskID(ctx, c, args[0])
		if err != nil {
			return err
		}
		resp, err := c.GetTaskWithResponse(ctx, id)
		if err := s.check(resp, err, http.StatusOK); err != nil {
			return err
		}
		var task client.Task
		if err := json.Unmarshal(resp.Body, &task); err != nil {
			return err
		}
		d := resp.JSON200
		if err := a.print(result{raw: resp.Body, ids: []string{id.String()}, table: func(w io.Writer) {
			taskCard(w, task)
			if comments {
				_, _ = fmt.Fprintln(w, "\ncomments (newest first):")
				if len(d.Comments) == 0 {
					_, _ = fmt.Fprintln(w, "  none")
				}
				for _, cm := range d.Comments {
					_, _ = fmt.Fprintf(w, "  %s  %s: %s\n", fmtTime(cm.CreatedAt), cm.AuthorId, cm.Body)
				}
			}
		}}); err != nil {
			return err
		}
		// The lists are separate endpoints, printed after the task, each as
		// its own body under --json.
		if subtasks {
			resp, err := c.ListSubtasksWithResponse(ctx, id)
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			st := resp.JSON200.Subtasks
			if err := a.print(result{raw: resp.Body, table: func(w io.Writer) {
				_, _ = fmt.Fprintln(w, "\nsubtasks:")
				if len(st) == 0 {
					_, _ = fmt.Fprintln(w, "  none")
					return
				}
				tasks := make([]client.Task, len(st))
				for i, x := range st {
					tasks[i] = x.Task
				}
				taskTable(w, tasks)
			}}); err != nil {
				return err
			}
		}
		if runs {
			resp, err := c.ListAgentRunsWithResponse(ctx, id)
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			return a.print(result{raw: resp.Body, table: func(w io.Writer) {
				_, _ = fmt.Fprintln(w, "\nagent runs:")
				if len(resp.JSON200.AgentRuns) == 0 {
					_, _ = fmt.Fprintln(w, "  none")
				}
				for _, j := range resp.JSON200.AgentRuns {
					_, _ = fmt.Fprintf(w, "  job %s  %s  %s  (%d turns)\n", shortID(j.Job.Id.String()), j.Job.AgentType, j.Job.Status, len(j.Runs))
				}
			}})
		}
		return nil
	})
	return cmd
}

// patchTask reads the task, applies change, and sends the new fields with the
// version it read in If-Match. A 409 is reported with the current version and
// is not retried: the person decides whether the change still makes sense.
func (s *session) patchTask(ctx context.Context, c *client.ClientWithResponses, id uuid.UUID, change map[string]any, precheck func(*client.TaskDetail) error) ([]byte, *client.Task, error) {
	cur, err := c.GetTaskWithResponse(ctx, id)
	if err := s.check(cur, err, http.StatusOK); err != nil {
		return nil, nil, err
	}
	if precheck != nil {
		if err := precheck(cur.JSON200); err != nil {
			return nil, nil, err
		}
	}
	body, err := json.Marshal(change)
	if err != nil {
		return nil, nil, err
	}
	ifMatch := fmt.Sprintf(`"%d"`, cur.JSON200.Version)
	resp, err := c.UpdateTaskWithBodyWithResponse(ctx, id, &client.UpdateTaskParams{IfMatch: &ifMatch}, "application/json", bytes.NewReader(body))
	if err := s.check(resp, err, http.StatusOK); err != nil {
		var ee *exitError
		if errors.As(err, &ee) && ee.code == exitConflict {
			return nil, nil, fail(exitConflict, "%s. Nothing was changed; run the command again to apply it to the new version", ee.msg)
		}
		return nil, nil, err
	}
	return resp.Body, resp.JSON200, nil
}

func (a *app) taskUpdate() *cobra.Command {
	var title, desc, priority, assignee, due, status string
	cmd := &cobra.Command{Use: "update <id>", Short: "Change fields of a task", Args: cobra.ExactArgs(1)}
	f := cmd.Flags()
	f.StringVar(&title, "title", "", "new title")
	f.StringVar(&desc, "desc", "", "new description")
	f.StringVar(&priority, "priority", "", "p1_critical, p2_high, p3_medium, or p4_low")
	f.StringVar(&assignee, "assignee", "", "assignee user ID; empty unassigns")
	f.StringVar(&due, "due", "", "due date; empty clears it")
	f.StringVar(&status, "status", "", "todo, in_progress, blocked, or completed")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
		change := map[string]any{}
		for flag, field := range map[string]string{"title": "title", "desc": "description", "priority": "priority", "status": "status"} {
			if f.Changed(flag) {
				v, _ := f.GetString(flag)
				change[field] = v
			}
		}
		if f.Changed("assignee") {
			change["assignee_id"] = nilIfEmpty(assignee)
		}
		if f.Changed("due") {
			if due == "" {
				change["due_at"] = nil
			} else {
				t, err := parseTime(due)
				if err != nil {
					return err
				}
				change["due_at"] = t.Format(time.RFC3339)
			}
		}
		if len(change) == 0 {
			return fail(exitUsage, "nothing to update: give at least one of --title, --desc, --priority, --assignee, --due, --status")
		}
		id, err := s.resolveTaskID(ctx, c, args[0])
		if err != nil {
			return err
		}
		raw, task, err := s.patchTask(ctx, c, id, change, nil)
		if err != nil {
			return err
		}
		return a.printTask(raw, task)
	})
	return cmd
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (a *app) taskDone() *cobra.Command {
	return &cobra.Command{Use: "done <id>", Short: "Complete a task", Args: cobra.ExactArgs(1),
		RunE: a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
			id, err := s.resolveTaskID(ctx, c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.CompleteTaskWithBodyWithResponse(ctx, id, &client.CompleteTaskParams{}, "application/json", strings.NewReader("{}"))
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			return a.printTask(resp.Body, resp.JSON200)
		})}
}

func (a *app) taskBlock() *cobra.Command {
	var reason string
	cmd := &cobra.Command{Use: "block <id>", Short: "Mark a task blocked", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&reason, "reason", "", "why; recorded as a comment on the task")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
		id, err := s.resolveTaskID(ctx, c, args[0])
		if err != nil {
			return err
		}
		raw, task, err := s.patchTask(ctx, c, id, map[string]any{"status": "blocked"}, nil)
		if err != nil {
			return err
		}
		if reason != "" {
			// The API has no reason field (blocked_reason is manual or
			// dependency), so the reason goes in as a comment (D65).
			resp, err := c.AddCommentWithResponse(ctx, id, client.CommentCreate{Body: "Blocked: " + reason})
			if err := s.check(resp, err, http.StatusCreated); err != nil {
				return fmt.Errorf("the task is blocked, but the reason could not be recorded: %w", err)
			}
		}
		return a.printTask(raw, task)
	})
	return cmd
}

func (a *app) taskUnblock() *cobra.Command {
	return &cobra.Command{Use: "unblock <id>", Short: "Move a blocked task back to in progress", Args: cobra.ExactArgs(1),
		RunE: a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
			id, err := s.resolveTaskID(ctx, c, args[0])
			if err != nil {
				return err
			}
			raw, task, err := s.patchTask(ctx, c, id, map[string]any{"status": "in_progress"}, func(t *client.TaskDetail) error {
				// The server would move any task to in_progress, a completed
				// one included; "unblock" must not reopen work by accident.
				if t.Status != client.Blocked {
					return fail(exitUsage, "task %s is %s, not blocked", shortID(id.String()), t.Status)
				}
				return nil
			})
			if err != nil {
				return err
			}
			return a.printTask(raw, task)
		})}
}

func (a *app) taskArchive() *cobra.Command {
	return &cobra.Command{Use: "archive <id>", Short: "Archive a task", Args: cobra.ExactArgs(1),
		RunE: a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
			id, err := s.resolveTaskID(ctx, c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.DeleteTaskWithResponse(ctx, id, &client.DeleteTaskParams{})
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			return a.printTask(resp.Body, resp.JSON200)
		})}
}

func (a *app) taskComment() *cobra.Command {
	return &cobra.Command{Use: "comment <id> <text>", Short: "Add a comment to a task", Args: cobra.ExactArgs(2),
		RunE: a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
			id, err := s.resolveTaskID(ctx, c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.AddCommentWithResponse(ctx, id, client.CommentCreate{Body: args[1]})
			if err := s.check(resp, err, http.StatusCreated); err != nil {
				return err
			}
			cm := resp.JSON201
			return a.print(result{raw: resp.Body, ids: []string{cm.Id.String()}, table: func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "comment %s added to task %s\n", cm.Id, shortID(id.String()))
			}})
		})}
}

func (a *app) taskDecompose() *cobra.Command {
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{Use: "decompose <id>", Short: "Ask the decomposer agent to split a task", Args: cobra.ExactArgs(1)}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the job finishes or waits for approval")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "with --wait, give up after this long")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
		id, err := s.resolveTaskID(ctx, c, args[0])
		if err != nil {
			return err
		}
		resp, err := c.DecomposeTaskWithResponse(ctx, id)
		if err := s.check(resp, err, http.StatusAccepted); err != nil {
			return err
		}
		j := resp.JSON202
		if !wait {
			return a.print(result{raw: resp.Body, ids: []string{j.Id.String()}, table: func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "decomposition queued: job %s (%s); follow it with: taskctl job show --watch %s\n", j.Id, j.Status, j.Id)
			}})
		}
		progress(a, "decomposition queued as job %s; waiting", j.Id)
		last, body, err := a.followJob(ctx, s, c, j.Id, timeout, func([]byte, *client.AgentJob) error { return nil })
		if err != nil {
			return err
		}
		if err := a.print(result{raw: body, ids: []string{last.Id.String()}, table: func(w io.Writer) { jobCard(w, *last) }}); err != nil {
			return err
		}
		if last.Status == client.AgentJobStatusAwaitingApproval {
			progress(a, "the plan waits for approval: taskctl job approve %s", last.Id)
		}
		return finished(last)
	})
	return cmd
}
