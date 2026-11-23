package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// login stores a pasted token (owner action O2 is deferred, so there is no
// sign-in flow; a token from cmd/devtoken works). The token is read from
// standard input, never from a flag, and is checked against GET /me before
// it is stored.
func (a *app) newLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Store a token for a profile",
		Long: "Reads a token from standard input (hidden when it is a terminal), checks it against\n" +
			"the server, and stores it in the operating system keychain, or in a file only you can\n" +
			"read when there is no keychain. Set the server first with --server or\n" +
			"'taskctl config set server URL'. Example: taskctl login --profile dev --server http://localhost:8080",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.session()
			if err != nil {
				return err
			}
			u, err := a.serverURL(s)
			if err != nil {
				return err
			}
			tok, err := a.readToken()
			if err != nil {
				return err
			}
			a.red.add(tok)
			c, err := a.clientFor(u, tok)
			if err != nil {
				return err
			}
			resp, err := c.GetMeWithResponse(cmd.Context())
			if err := s.check(resp, err, http.StatusOK); err != nil {
				return err
			}
			me := resp.JSON200
			if me == nil {
				return fail(exitUnexpected, "the server's answer to /me was not understood")
			}
			where, err := s.creds.Save(s.profile, tok)
			if err != nil {
				return failure(err)
			}
			s.cfg.set(s.profile, "server", s.server)
			s.cfg.set(s.profile, "tenant", me.TenantId)
			if s.cfg.v.GetString("current_profile") == "" {
				s.cfg.v.Set("current_profile", s.profile)
			}
			if err := s.cfg.save(); err != nil {
				return failure(err)
			}
			_, _ = fmt.Fprintf(a.stdout, "Logged in to %s as %s (%s) in tenant %s.\nCredentials for profile %q are stored in the %s.\n",
				s.server, me.UserId, me.Role, me.TenantId, s.profile, where)
			return nil
		},
	}
}

// readToken reads one line from stdin, without echo on a terminal.
func (a *app) readToken() (string, error) {
	var line string
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprint(a.stderr, "Token: ")
		b, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(a.stderr)
		if err != nil {
			return "", fail(exitUnexpected, "could not read the token: %v", err)
		}
		line = string(b)
	} else {
		l, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && l == "" {
			return "", fail(exitUsage, "no token on standard input")
		}
		line = l
	}
	if line = strings.TrimSpace(line); line == "" {
		return "", fail(exitUsage, "the token is empty")
	}
	return line, nil
}
