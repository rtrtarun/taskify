package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (a *app) newVersionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the taskctl version", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return a.note(map[string]string{"version": version}, fmt.Sprintf("taskctl %s", version))
		}}
}
