package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/rtrtarun/taskify/pkg/client"
)

// app holds what commands share. Tests build one with their own streams,
// environment, and keychain.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	keychain       keychain
	httpClient     *http.Client

	red *redactor

	// Global flags.
	profileFlag, serverFlag, workspaceFlag string
	verbose, jsonOut, quiet                bool

	// pollEvery is how often --watch and --wait ask the server again.
	pollEvery time.Duration
	// versionNote makes the newer-server notice appear once per run.
	versionNote sync.Once
	// ctx is cancelled by Ctrl-C (set in main).
	ctx context.Context
}

func newApp(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) *app {
	a := &app{stdin: stdin, stdout: stdout, getenv: getenv, keychain: osKeychain{}, red: &redactor{}, pollEvery: 2 * time.Second, ctx: context.Background()}
	a.stderr = &redactWriter{w: stderr, r: a.red}
	return a
}

// redactWriter cleans each write; messages are written whole, so a token is
// never split across two writes.
type redactWriter struct {
	w io.Writer
	r *redactor
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(rw.w, rw.r.clean(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// run executes one command line and returns the process exit code.
func (a *app) run(args []string) int {
	root := a.newRoot()
	root.SetArgs(args)
	err := root.ExecuteContext(a.ctx)
	if err == nil {
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		_, _ = fmt.Fprintf(a.stderr, "taskctl: %s\n", ee.msg)
		return ee.code
	}
	// Not ours, so cobra's: an unknown command or flag, or a wrong number
	// of arguments.
	_, _ = fmt.Fprintf(a.stderr, "taskctl: %s\n(see taskctl --help)\n", err)
	return exitUsage
}

func (a *app) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "taskctl",
		Short:         "Command-line client for Taskify",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	f := root.PersistentFlags()
	f.StringVar(&a.profileFlag, "profile", "", "profile to use (env TASKIFY_PROFILE)")
	f.StringVar(&a.serverFlag, "server", "", "server URL (env TASKIFY_SERVER)")
	f.BoolVar(&a.verbose, "verbose", false, "trace requests on stderr (method, URL, status; never credentials)")
	f.BoolVar(&a.jsonOut, "json", false, "print the API's response body unchanged, for scripts")
	f.BoolVarP(&a.quiet, "quiet", "q", false, "print only IDs")
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		if a.jsonOut && a.quiet {
			return fail(exitUsage, "--json and --quiet cannot be used together")
		}
		return nil
	}
	root.AddCommand(a.newVersionCmd(), a.newLoginCmd(), a.newConfigCmd(), a.newTaskCmd(), a.newJobCmd(), a.newWorkspaceCmd(), a.newAdminCmd())
	return root
}

// session is the resolved configuration for a command that calls the API.
type session struct {
	settings
	cfg   *configStore
	creds credStore
}

func (a *app) credStore(cfg *configStore) credStore {
	return credStore{keychain: a.keychain, file: filepath.Join(filepath.Dir(cfg.path), "credentials.json")}
}

func (a *app) session() (*session, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return nil, failure(err)
	}
	s, err := a.resolve(cfg)
	if err != nil {
		return nil, err
	}
	return &session{settings: s, cfg: cfg, creds: a.credStore(cfg)}, nil
}

// token is TASKIFY_TOKEN, else the profile's stored credential. There is no
// token flag: a flag shows in the process list and shell history.
func (a *app) token(s *session) (string, error) {
	tok := a.getenv("TASKIFY_TOKEN")
	if tok == "" {
		var err error
		if tok, _, err = s.creds.Load(s.profile); err != nil {
			return "", failure(err)
		}
	}
	if tok == "" {
		return "", fail(exitAuth, "no credentials for profile %q; run: taskctl login", s.profile)
	}
	a.red.add(tok)
	return tok, nil
}

// newClient builds the API client for the session. Nothing it logs or
// returns holds the token.
func (a *app) newClient(s *session) (*client.ClientWithResponses, error) {
	u, err := a.serverURL(s)
	if err != nil {
		return nil, err
	}
	tok, err := a.token(s)
	if err != nil {
		return nil, err
	}
	return a.clientFor(u, tok)
}

func (a *app) serverURL(s *session) (*url.URL, error) {
	if s.server == "" {
		return nil, fail(exitUsage, "no server set for profile %q; use --server, TASKIFY_SERVER, or: taskctl config set server URL", s.profile)
	}
	u, err := url.Parse(s.server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fail(exitUsage, "server %q is not an http or https URL", s.server)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		_, _ = fmt.Fprintln(a.stderr, "taskctl: warning: this server is plain http, so the token travels unencrypted")
	}
	return u, nil
}

func (a *app) clientFor(u *url.URL, tok string) (*client.ClientWithResponses, error) {
	hc := a.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if a.verbose {
		hc = &http.Client{Timeout: hc.Timeout, Transport: &traceTransport{next: hc.Transport, out: a.stderr}}
	}
	hc = &http.Client{Timeout: hc.Timeout, Transport: &versionTransport{next: hc.Transport, app: a}}
	auth := func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("User-Agent", "taskctl/"+version)
		return nil
	}
	return client.NewClientWithResponses(u.String(), client.WithHTTPClient(hc), client.WithRequestEditorFn(auth))
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// versionTransport watches X-Taskify-API-Version. When the server's API is
// newer than the one this build of taskctl was generated from, it says so on
// stderr, once per run. It is a notice, not an error: the command goes on,
// and a client generated from an older version of the same major version
// still works.
type versionTransport struct {
	next http.RoundTripper
	app  *app
}

func (t *versionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(r)
	if err == nil {
		if v := resp.Header.Get(client.HeaderAPIVersion); client.ServerIsNewer(v) {
			t.app.versionNote.Do(func() {
				_, _ = fmt.Fprintf(t.app.stderr, "taskctl: note: the server's API version is %s, newer than the %s this taskctl was built for; "+
					"some fields or commands may be missing (upgrade taskctl)\n", v, client.GeneratedAPIVersion())
			})
		}
	}
	return resp, err
}

// traceTransport is the --verbose trace. It writes the method, URL, and
// status, and nothing else of the request or response: no headers and no
// bodies, so no credential can be in it.
type traceTransport struct {
	next http.RoundTripper
	out  io.Writer
}

func (t *traceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	start := time.Now()
	_, _ = fmt.Fprintf(t.out, "> %s %s\n", r.Method, r.URL.Redacted())
	resp, err := next.RoundTrip(r)
	if err != nil {
		_, _ = fmt.Fprintf(t.out, "< failed after %s\n", time.Since(start).Round(time.Millisecond))
		return nil, err
	}
	_, _ = fmt.Fprintf(t.out, "< %s in %s\n", resp.Status, time.Since(start).Round(time.Millisecond))
	return resp, nil
}

// apiFailure turns a response that is not the expected success into an
// exitError. A transport error (no response) is exit 6.
func apiFailure(status int, body []byte) error {
	var e struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	code := statusExit(status, e.Error.Code)
	text := fmt.Sprintf("%s (HTTP %d)", msg, status)
	if code == exitConflict {
		if cv, ok := e.Error.Details["current_version"].(float64); ok {
			text = fmt.Sprintf("%s; the task is now at version %d", text, int(cv))
		}
	}
	return fail(code, "%s", text)
}

// transportFailure classifies an error from the client with no response.
func transportFailure(err error, server string) error {
	if errors.Is(err, context.Canceled) {
		return fail(exitUnexpected, "interrupted")
	}
	// err can quote the URL but never a header, so it holds no token; the
	// redacting stderr would catch one anyway.
	return fail(exitUnreachable, "cannot reach %s: %v", server, err)
}

// check is the shared tail of a call. resp is the pointer to a generated
// response (every one has a Body and an HTTPResponse). A transport error, or
// a status other than want, becomes an exitError.
func (s *session) check(resp any, err error, want ...int) error {
	if err != nil {
		return transportFailure(err, s.server)
	}
	v := reflect.ValueOf(resp).Elem()
	status := v.FieldByName("HTTPResponse").Interface().(*http.Response).StatusCode
	for _, w := range want {
		if status == w {
			return nil
		}
	}
	return apiFailure(status, bytes.TrimSpace(v.FieldByName("Body").Bytes()))
}
