package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rtrtarun/taskify/pkg/client"
)

// The admin commands call admin endpoints. Whether the caller is an admin is
// the server's decision: a member gets 403, which is exit code 3.
func (a *app) newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Administration (admins only)"}

	suggestions := &cobra.Command{Use: "suggestions", Short: "Learning suggestions from AgentPulse"}
	suggestions.AddCommand(&cobra.Command{Use: "list", Short: "List the tenant's learning suggestions", Args: cobra.NoArgs,
		RunE: a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, _ []string) error {
			resp, err := c.ListLearningSuggestionsWithResponse(ctx)
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			list := resp.JSON200.Suggestions
			ids := make([]string, len(list))
			rows := make([][]string, len(list))
			for i, sg := range list {
				scope := "-"
				switch {
				case sg.RuleId != nil:
					scope = "rule " + *sg.RuleId
				case sg.EventPattern != nil:
					scope = sg.EventPattern.EventType
					if sg.EventPattern.Label != nil {
						scope += " / " + *sg.EventPattern.Label
					}
				}
				ids[i] = sg.Id
				rows[i] = []string{sg.Id, sg.Status, scope, fmt.Sprintf("%+d -> %+d", sg.CurrentAdjustment, sg.ProposedAdjustment), fmt.Sprint(sg.SignalCount), sg.Rationale}
			}
			return a.print(result{raw: resp.Body, ids: ids, table: func(w io.Writer) {
				tabular(w, []string{"ID", "STATUS", "SCOPE", "ADJUSTMENT", "SIGNALS", "RATIONALE"}, rows)
			}})
		})})

	var approve, reject bool
	decide := &cobra.Command{Use: "decide <id>", Short: "Approve or reject a learning suggestion", Args: cobra.ExactArgs(1)}
	decide.Flags().BoolVar(&approve, "approve", false, "approve the suggestion")
	decide.Flags().BoolVar(&reject, "reject", false, "reject the suggestion")
	decide.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
		if approve == reject {
			return fail(exitUsage, "give exactly one of --approve and --reject")
		}
		d := client.LearningDecisionDecision("reject")
		if approve {
			d = "approve"
		}
		resp, err := c.DecideLearningSuggestionWithResponse(ctx, args[0], client.LearningDecision{Decision: d})
		if err := s.check(resp, err, http.StatusOK); err != nil {
			return err
		}
		sg := resp.JSON200
		return a.print(result{raw: resp.Body, ids: []string{sg.Id}, table: func(w io.Writer) {
			_, _ = fmt.Fprintf(w, "suggestion %s is now %s\n", sg.Id, sg.Status)
		}})
	})
	suggestions.AddCommand(decide)

	rules := &cobra.Command{Use: "rules", Short: "AgentPulse rules"}
	rules.AddCommand(&cobra.Command{Use: "bootstrap <workspace>", Short: "Register a workspace's default rules (safe to repeat)", Args: cobra.ExactArgs(1),
		RunE: a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
			resp, err := c.BootstrapWorkspaceRulesWithResponse(ctx, args[0])
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			list := resp.JSON200.Rules
			ids := make([]string, len(list))
			rows := make([][]string, len(list))
			for i, r := range list {
				ids[i], rows[i] = r.AgentpulseRuleId, []string{r.Key, r.AgentpulseRuleId, fmtTime(r.CreatedAt)}
			}
			return a.print(result{raw: resp.Body, ids: ids, table: func(w io.Writer) {
				tabular(w, []string{"RULE", "AGENTPULSE ID", "CREATED"}, rows)
			}})
		})})

	outbox := &cobra.Command{Use: "outbox", Short: "Notifications waiting for AgentPulse"}
	var status string
	var limit int
	list := &cobra.Command{Use: "list", Short: "List outbox rows, newest first", Args: cobra.NoArgs}
	list.Flags().StringVar(&status, "status", "", "pending, processing, dispatched, or dead_letter")
	list.Flags().IntVar(&limit, "limit", 50, "how many rows, 1 to 200")
	list.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, _ []string) error {
		p := &client.ListOutboxParams{}
		if list.Flags().Changed("limit") {
			p.Limit = &limit
		}
		if status != "" {
			st := client.OutboxStatus(status)
			p.Status = &st
		}
		resp, err := c.ListOutboxWithResponse(ctx, p)
		if err := s.check(resp, err, http.StatusOK); err != nil {
			return err
		}
		entries := resp.JSON200.Entries
		ids := make([]string, len(entries))
		rows := make([][]string, len(entries))
		for i, e := range entries {
			ids[i] = e.Id.String()
			rows[i] = []string{e.Id.String(), e.EventType, string(e.Status), fmt.Sprintf("%d/%d", e.Attempts, e.MaxAttempts), dash(strings.TrimSpace(ptrStr(e.LastError)))}
		}
		if err := a.print(result{raw: resp.Body, ids: ids, table: func(w io.Writer) {
			tabular(w, []string{"ID", "EVENT", "STATUS", "ATTEMPTS", "LAST ERROR"}, rows)
		}}); err != nil {
			return err
		}
		if resp.JSON200.NextCursor != nil {
			_, _ = fmt.Fprintln(a.stderr, "taskctl: more rows exist; raise --limit to see them")
		}
		return nil
	})
	replay := &cobra.Command{Use: "replay <id>", Short: "Send a dead-letter row again", Args: cobra.ExactArgs(1),
		RunE: a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, args []string) error {
			id, err := parseJobID(args[0]) // any full UUID
			if err != nil {
				return fail(exitUsage, "%q is not an outbox row ID (the full ID, as 'taskctl admin outbox list' shows it)", args[0])
			}
			resp, err := c.ReplayOutboxEntryWithResponse(ctx, id)
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			e := resp.JSON200
			return a.print(result{raw: resp.Body, ids: []string{e.Id.String()}, table: func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "outbox row %s is %s again\n", e.Id, e.Status)
			}})
		})}
	outbox.AddCommand(list, replay)

	cmd.AddCommand(suggestions, rules, outbox)
	return cmd
}
