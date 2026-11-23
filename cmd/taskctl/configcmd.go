package main

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"
)

func (a *app) newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "View and change the configuration"}

	view := &cobra.Command{
		Use:   "view",
		Short: "Show the settings in effect and where each comes from",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s, err := a.session()
			if err != nil {
				return err
			}
			// The token is reported as set or not, never shown.
			cred := "not set"
			switch {
			case a.getenv("TASKIFY_TOKEN") != "":
				cred = "set (environment)"
			default:
				if tok, where, err := s.creds.Load(s.profile); err != nil {
					return failure(err)
				} else if tok != "" {
					cred = "set (" + where + ")"
				}
			}
			if a.jsonOut {
				return a.note(map[string]any{"config_file": s.cfg.path, "profile": s.profile, "server": s.server,
					"tenant": s.tenant, "workspace": s.workspace, "credentials": cred, "profiles": s.cfg.profiles()}, "")
			}
			w := a.stdout
			_, _ = fmt.Fprintf(w, "config file: %s\n", s.cfg.path)
			_, _ = fmt.Fprintf(w, "profile:     %s (%s)\n", s.profile, s.sources["profile"])
			_, _ = fmt.Fprintf(w, "server:      %s (%s)\n", orNone(s.server), s.sources["server"])
			_, _ = fmt.Fprintf(w, "tenant:      %s (%s)\n", orNone(s.tenant), s.sources["tenant"])
			_, _ = fmt.Fprintf(w, "workspace:   %s (%s)\n", orNone(s.workspace), s.sources["workspace"])
			_, _ = fmt.Fprintf(w, "credentials: %s\n", cred)
			_, _ = fmt.Fprintf(w, "profiles:    %v\n", s.cfg.profiles())
			return nil
		},
	}

	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set server, tenant, or workspace for the profile",
		Long: "Keys: server, tenant, workspace. The credential is not a setting; store it with 'taskctl login'.\n" +
			"Applies to --profile, TASKIFY_PROFILE, or the current profile.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if !slices.Contains(profileKeys, args[0]) {
				return fail(exitUsage, "unknown key %q; keys are %v (credentials are stored by 'taskctl login')", args[0], profileKeys)
			}
			s, err := a.session()
			if err != nil {
				return err
			}
			s.cfg.set(s.profile, args[0], args[1])
			if s.cfg.v.GetString("current_profile") == "" {
				s.cfg.v.Set("current_profile", s.profile)
			}
			if err := s.cfg.save(); err != nil {
				return failure(err)
			}
			return a.note(map[string]string{"profile": s.profile, "key": args[0], "value": args[1]},
				fmt.Sprintf("%s set for profile %q", args[0], s.profile))
		},
	}

	use := &cobra.Command{
		Use:   "use-profile <name>",
		Short: "Make a profile the current one",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !profileName.MatchString(args[0]) {
				return fail(exitUsage, "invalid profile name %q: use lower-case letters, digits, '-' and '_'", args[0])
			}
			cfg, err := a.loadConfig()
			if err != nil {
				return failure(err)
			}
			cfg.v.Set("current_profile", args[0])
			if err := cfg.save(); err != nil {
				return failure(err)
			}
			return a.note(map[string]string{"current_profile": args[0]}, fmt.Sprintf("current profile is now %q", args[0]))
		},
	}
	cmd.AddCommand(view, set, use)
	return cmd
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
