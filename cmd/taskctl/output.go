package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rtrtarun/taskify/pkg/client"
)

// result is what a command has to show: the API's body as it came, the IDs
// it concerns, and how to draw it for a person.
type result struct {
	raw   []byte
	ids   []string
	table func(w io.Writer)
}

// print writes a result to standard output in the mode the flags chose:
// --json, the API's body exactly as it came; --quiet, the IDs, one per line;
// otherwise a table or card for a person.
func (a *app) print(r result) error {
	switch {
	case a.jsonOut:
		_, err := a.stdout.Write(withNewline(r.raw))
		return err
	case a.quiet:
		for _, id := range r.ids {
			if _, err := fmt.Fprintln(a.stdout, id); err != nil {
				return err
			}
		}
		return nil
	}
	r.table(a.stdout)
	return nil
}

func withNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return append(append([]byte{}, b...), '\n')
	}
	return b
}

// note reports what a command with no API body did: v as JSON under --json,
// nothing under --quiet, text otherwise.
func (a *app) note(v any, text string) error {
	switch {
	case a.jsonOut:
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = a.stdout.Write(withNewline(raw))
		return err
	case a.quiet:
		return nil
	}
	_, err := fmt.Fprintln(a.stdout, text)
	return err
}

// tabular draws rows with aligned columns.
func tabular(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		_, _ = fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ts(t *client.NullableTimestamp) string {
	if t == nil {
		return "-"
	}
	return fmtTime(*t)
}

func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04Z") }

func ptrStr(s *string) string {
	if s == nil {
		return "-"
	}
	return dash(*s)
}

func taskRow(t client.Task) []string {
	return []string{shortID(t.Id.String()), string(t.Status), string(t.Priority), ptrStr(t.AssigneeId), ts(t.DueAt), t.Title}
}

func taskTable(w io.Writer, tasks []client.Task) {
	rows := make([][]string, len(tasks))
	for i, t := range tasks {
		rows[i] = taskRow(t)
	}
	tabular(w, []string{"ID", "STATUS", "PRIORITY", "ASSIGNEE", "DUE", "TITLE"}, rows)
}

// taskCard prints one task, field by field.
func taskCard(w io.Writer, t client.Task) {
	f := func(k, v string) { _, _ = fmt.Fprintf(w, "%-10s %s\n", k+":", v) }
	f("id", t.Id.String())
	f("title", t.Title)
	f("status", string(t.Status))
	if t.BlockedReason != nil {
		f("blocked", string(*t.BlockedReason))
	}
	f("priority", string(t.Priority))
	f("workspace", t.WorkspaceId)
	f("creator", t.CreatorId)
	f("assignee", ptrStr(t.AssigneeId))
	f("due", ts(t.DueAt))
	f("labels", dash(strings.Join(t.Labels, ", ")))
	f("version", fmt.Sprint(t.Version))
	f("updated", fmtTime(t.UpdatedAt))
	if t.Description != "" {
		_, _ = fmt.Fprintf(w, "\n%s\n", t.Description)
	}
}
