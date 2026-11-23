package main

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/rtrtarun/taskify/pkg/client"
)

func (a *app) newWorkspaceCmd() *cobra.Command {
	cmd := group(&cobra.Command{Use: "workspace", Short: "The tenant's workspaces"})
	cmd.AddCommand(a.workspaceList())
	return cmd
}

func (a *app) workspaceList() *cobra.Command {
	var limit int
	cmd := &cobra.Command{Use: "list", Short: "List the tenant's workspaces", Args: cobra.NoArgs}
	cmd.Flags().IntVar(&limit, "limit", 50, "how many workspaces, 1 to 200")
	cmd.RunE = a.api(func(ctx context.Context, s *session, c *client.ClientWithResponses, _ []string) error {
		p := &client.ListWorkspacesParams{}
		if cmd.Flags().Changed("limit") {
			p.Limit = &limit
		}
		resp, err := c.ListWorkspacesWithResponse(ctx, p)
		if err := s.check(resp, err, http.StatusOK); err != nil {
			return err
		}
		list := resp.JSON200.Workspaces
		ids := make([]string, len(list))
		rows := make([][]string, len(list))
		for i, w := range list {
			ids[i], rows[i] = w.Id, []string{w.Id, w.Name, fmtTime(w.CreatedAt)}
		}
		if err := a.print(result{raw: resp.Body, ids: ids, table: func(w io.Writer) {
			tabular(w, []string{"ID", "NAME", "CREATED"}, rows)
		}}); err != nil {
			return err
		}
		if resp.JSON200.NextCursor != nil {
			_, _ = fmt.Fprintln(a.stderr, "taskctl: more workspaces exist; raise --limit to see them")
		}
		return nil
	})
	return cmd
}
