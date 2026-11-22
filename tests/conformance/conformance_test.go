package conformance

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/tests/mockagentpulse"
	"github.com/rtrtarun/taskify/tests/mockllm"
)

// fixturesDir is contract/fixtures, from this package's directory.
const fixturesDir = "../../contract/fixtures"

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// TestConformance runs every fixture against TARGET_URL:
//
//	TARGET_URL=http://127.0.0.1:8091 go test ./tests/conformance/...
//
// The target must call the mocks this test starts: AGENTPULSE_BASE_URL at
// MOCK_AGENTPULSE_ADDR (default 127.0.0.1:9190) and LLM_BASE_URL at
// http://MOCK_LLM_ADDR/v1 (default 127.0.0.1:9191). CONFORMANCE_JWT_SECRET
// and CONFORMANCE_WEBHOOK_SECRET are the target's JWT_SECRET and
// AGENTPULSE_WEBHOOK_SECRET. contract/fixtures/README.md lists the rest of
// the target's settings. Without TARGET_URL the test is skipped.
func TestConformance(t *testing.T) {
	target := os.Getenv("TARGET_URL")
	if target == "" {
		t.Skip("TARGET_URL is not set; see contract/fixtures/README.md")
	}
	cfg := Config{
		TargetURL:     target,
		JWTSecret:     env("CONFORMANCE_JWT_SECRET", "conformance-jwt-secret"),
		WebhookSecret: env("CONFORMANCE_WEBHOOK_SECRET", "conformance-webhook-secret"),
	}
	ap, llm := mockagentpulse.New(), mockllm.New()
	serve(t, env("MOCK_AGENTPULSE_ADDR", "127.0.0.1:9190"), ap)
	serve(t, env("MOCK_LLM_ADDR", "127.0.0.1:9191"), llm)
	waitReady(t, target)

	fixtures, err := LoadFixtures(fixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(cfg, ap, llm)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) { runner.Run(t, f) })
	}
}

// serve starts a mock on addr for the length of the test.
func serve(t *testing.T, addr string, h http.Handler) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s for a mock: %v", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("mock on %s: %v", addr, err)
		}
	}()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// waitReady waits for the target's /readyz.
func waitReady(t *testing.T, target string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(target + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is not ready: %v", target, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
