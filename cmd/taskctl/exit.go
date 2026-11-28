package main

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

// Exit codes (docs/architecture.md section 9.5).
const (
	exitOK           = 0
	exitUnexpected   = 1
	exitUsage        = 2
	exitAuth         = 3
	exitNotFound     = 4
	exitConflict     = 5
	exitUnreachable  = 6
	codeVersionClash = "version_conflict"
	codeStaleVersion = "stale_version"
)

// exitError is an error that decides the process exit code. Anything else
// that reaches run is cobra's own (unknown flag, wrong argument count) and
// is a usage error.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, args...)}
}

// failure wraps an arbitrary error as an unexpected one (exit 1), keeping
// an exitError as it is.
func failure(err error) error {
	var ee *exitError
	if errors.As(err, &ee) {
		return err
	}
	return &exitError{code: exitUnexpected, msg: err.Error()}
}

// statusExit maps an API failure to an exit code. The server's error code,
// not its message, decides the 409 cases: only a version clash is code 5.
func statusExit(status int, errCode string) int {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return exitAuth
	case status == http.StatusNotFound:
		return exitNotFound
	case status == http.StatusConflict && (errCode == codeVersionClash || errCode == codeStaleVersion):
		return exitConflict
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge || status == http.StatusUnprocessableEntity:
		return exitUsage
	}
	return exitUnexpected
}

// group makes a command that only holds subcommands runnable, so that
// "taskctl task frobnicate" is a usage error (exit 2) instead of silently
// printing help and succeeding. With no arguments it prints help.
func group(cmd *cobra.Command) *cobra.Command {
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) > 0 {
			return fail(exitUsage, "unknown command %q for %q (see %s --help)", args[0], c.CommandPath(), c.CommandPath())
		}
		return c.Help()
	}
	return cmd
}
