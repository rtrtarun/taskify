package config

import (
	"strings"
	"testing"
	"time"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// valid returns the smallest environment that passes validation in production.
func valid() map[string]string {
	return map[string]string{
		"DATABASE_URL":              "postgres://u:p@localhost:5432/taskify",
		"PUBLIC_BASE_URL":           "https://taskify.example/",
		"AGENTPULSE_BASE_URL":       "http://agentpulse:9090",
		"AGENTPULSE_API_KEY":        "key",
		"AGENTPULSE_WEBHOOK_SECRET": "whsec",
		"JWT_SECRET":                "jwt-secret",
	}
}

func TestDefaults(t *testing.T) {
	c, err := FromLookup(lookupFrom(valid()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Env != EnvProduction {
		t.Errorf("Env = %q, want production", c.Env)
	}
	if c.IsDevelopment() {
		t.Error("IsDevelopment() = true by default")
	}
	if c.Port != 8080 {
		t.Errorf("Port = %d, want 8080", c.Port)
	}
	if c.PublicBaseURL != "https://taskify.example" {
		t.Errorf("PublicBaseURL = %q, want trailing slash trimmed", c.PublicBaseURL)
	}
	if c.OutboxWorkers != 8 || c.OutboxBatchSize != 100 || c.OutboxMaxAttempts != 10 {
		t.Errorf("outbox defaults = %d/%d/%d", c.OutboxWorkers, c.OutboxBatchSize, c.OutboxMaxAttempts)
	}
	if c.OutboxPollInterval != 200*time.Millisecond {
		t.Errorf("OutboxPollInterval = %v", c.OutboxPollInterval)
	}
	if c.OutboxLease != time.Minute || c.SchedulerInterval != time.Minute {
		t.Errorf("lease/scheduler = %v/%v", c.OutboxLease, c.SchedulerInterval)
	}
	if c.OutboxRetryMaxDelay != time.Minute {
		t.Errorf("OutboxRetryMaxDelay = %v", c.OutboxRetryMaxDelay)
	}
	if c.JWTAlgorithm != JWTAlgHS256 {
		t.Errorf("JWTAlgorithm = %q", c.JWTAlgorithm)
	}
	if c.RateLimitRPS != 20 || c.RateLimitBurst != 40 {
		t.Errorf("rate limit defaults = %v/%d", c.RateLimitRPS, c.RateLimitBurst)
	}
	if len(c.TrustedGatewayCIDRs) != 0 {
		t.Errorf("TrustedGatewayCIDRs = %v, want empty", c.TrustedGatewayCIDRs)
	}
}

func TestOverrides(t *testing.T) {
	env := valid()
	env["ENV"] = "development"
	env["PORT"] = "9000"
	env["OUTBOX_POLL_INTERVAL"] = "1s"
	env["TRUSTED_GATEWAY_CIDRS"] = "10.0.0.0/8, 192.168.1.7/24"
	env["RATE_LIMIT_RPS"] = "2.5"
	env["OUTBOX_RETRY_MAX_DELAY"] = "100ms"

	c, err := FromLookup(lookupFrom(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsDevelopment() || c.Port != 9000 || c.OutboxPollInterval != time.Second || c.RateLimitRPS != 2.5 ||
		c.OutboxRetryMaxDelay != 100*time.Millisecond {
		t.Errorf("overrides not applied: %+v", c)
	}
	if len(c.TrustedGatewayCIDRs) != 2 || c.TrustedGatewayCIDRs[1].String() != "192.168.1.0/24" {
		t.Errorf("TrustedGatewayCIDRs = %v", c.TrustedGatewayCIDRs)
	}
}

func TestRequiredVariables(t *testing.T) {
	for _, name := range []string{
		"DATABASE_URL", "PUBLIC_BASE_URL", "AGENTPULSE_BASE_URL",
		"AGENTPULSE_API_KEY", "AGENTPULSE_WEBHOOK_SECRET", "JWT_SECRET",
	} {
		t.Run(name, func(t *testing.T) {
			env := valid()
			delete(env, name)
			_, err := FromLookup(lookupFrom(env))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("error = %v, want one naming %s", err, name)
			}
		})
	}
}

func TestInvalidValues(t *testing.T) {
	cases := []struct{ name, value string }{
		{"ENV", "staging"},
		{"PORT", "http"},
		{"PORT", "70000"},
		{"PUBLIC_BASE_URL", "taskify.example"},
		{"APP_BASE_URL", "app.taskify.example"},
		{"APP_BASE_URL", "mailto:app@taskify.example"},
		{"AGENTPULSE_BASE_URL", "ftp://agentpulse"},
		{"OUTBOX_WORKERS", "0"},
		{"OUTBOX_POLL_INTERVAL", "fast"},
		{"OUTBOX_POLL_INTERVAL", "-1s"},
		{"SCHEDULER_INTERVAL", "60"},
		{"JWT_ALGORITHM", "none"},
		{"TRUSTED_GATEWAY_CIDRS", "10.0.0.1"},
		{"RATE_LIMIT_RPS", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			env := valid()
			env[tc.name] = tc.value
			_, err := FromLookup(lookupFrom(env))
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("error = %v, want one naming %s", err, tc.name)
			}
		})
	}
}

func TestJWTKeyRules(t *testing.T) {
	t.Run("RS256 in production needs the public key", func(t *testing.T) {
		env := valid()
		env["JWT_ALGORITHM"] = "RS256"
		_, err := FromLookup(lookupFrom(env))
		if err == nil || !strings.Contains(err.Error(), "JWT_PUBLIC_KEY") {
			t.Fatalf("error = %v, want one naming JWT_PUBLIC_KEY", err)
		}
		env["JWT_PUBLIC_KEY"] = "-----BEGIN PUBLIC KEY-----"
		if _, err := FromLookup(lookupFrom(env)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("development may run without a key", func(t *testing.T) {
		env := valid()
		env["ENV"] = "development"
		delete(env, "JWT_SECRET")
		if _, err := FromLookup(lookupFrom(env)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestAllProblemsReportedTogether(t *testing.T) {
	_, err := FromLookup(lookupFrom(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range []string{"DATABASE_URL", "AGENTPULSE_API_KEY", "JWT_SECRET"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not mention %s: %v", name, err)
		}
	}
}

func TestSecretsAreNotExposed(t *testing.T) {
	env := valid()
	env["JWT_SECRET"] = "super-secret-value"
	env["AGENTPULSE_API_KEY"] = "ap-key-value"
	env["PORT"] = "bad"

	_, err := FromLookup(lookupFrom(env))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "super-secret-value") || strings.Contains(err.Error(), "ap-key-value") {
		t.Errorf("error leaks a secret: %v", err)
	}

	env["PORT"] = "8080"
	c, err := FromLookup(lookupFrom(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := c.String(); strings.Contains(s, "super-secret-value") || strings.Contains(s, "ap-key-value") {
		t.Errorf("String() leaks a secret: %s", s)
	}
}

func TestAgentSettings(t *testing.T) {
	c, err := FromLookup(lookupFrom(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentsEnabled || c.AgentWorkers != 4 || c.AgentMaxTurns != 6 || c.AgentMaxAttempts != 3 || c.LLMTimeout != 30*time.Second {
		t.Errorf("agent defaults = %+v", c)
	}
	if c.DigestLoadThreshold != 3 || c.DigestBriefHourUTC != 8 {
		t.Errorf("digest defaults = %+v", c)
	}

	// Turning agents on needs a model endpoint and a model name.
	env := valid()
	env["AGENTS_ENABLED"] = "true"
	_, err = FromLookup(lookupFrom(env))
	if err == nil || !strings.Contains(err.Error(), "LLM_BASE_URL") || !strings.Contains(err.Error(), "LLM_MODEL") {
		t.Errorf("agents on without a model: %v", err)
	}

	env["LLM_BASE_URL"] = "http://ollama:11434/v1/"
	env["LLM_MODEL"] = "qwen2.5-coder:7b"
	env["LLM_API_KEY"] = "llm-secret-key"
	env["AGENT_WORKERS"] = "2"
	env["AGENT_MAX_TURNS"] = "4"
	env["LLM_TIMEOUT"] = "10s"
	env["DIGEST_LOAD_THRESHOLD"] = "5"
	env["DIGEST_BRIEF_HOUR_UTC"] = "0"
	c, err = FromLookup(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AgentsEnabled || c.LLMBaseURL != "http://ollama:11434/v1" || c.AgentWorkers != 2 || c.AgentMaxTurns != 4 || c.LLMTimeout != 10*time.Second {
		t.Errorf("agent config = %+v", c)
	}
	if c.DigestLoadThreshold != 5 || c.DigestBriefHourUTC != 0 {
		t.Errorf("digest config = %+v", c)
	}
	if strings.Contains(c.String(), "llm-secret-key") {
		t.Errorf("String() leaks the LLM key: %s", c.String())
	}

	for name, value := range map[string]string{"AGENTS_ENABLED": "maybe", "LLM_BASE_URL": "ollama:11434", "AGENT_MAX_TURNS": "0",
		"DIGEST_LOAD_THRESHOLD": "0", "DIGEST_BRIEF_HOUR_UTC": "24"} {
		bad := valid()
		bad[name] = value
		if _, err := FromLookup(lookupFrom(bad)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: %v", name, value, err)
		}
	}
}

// TestAppBaseURL is AP-10: APP_BASE_URL is optional and, when set, an
// absolute URL without its trailing slash.
func TestAppBaseURL(t *testing.T) {
	c, err := FromLookup(lookupFrom(valid()))
	if err != nil || c.AppBaseURL != "" {
		t.Fatalf("unset: %q, %v", c.AppBaseURL, err)
	}
	env := valid()
	env["APP_BASE_URL"] = " https://app.taskify.example/ "
	c, err = FromLookup(lookupFrom(env))
	if err != nil || c.AppBaseURL != "https://app.taskify.example" {
		t.Fatalf("set: %q, %v", c.AppBaseURL, err)
	}
}

// TestCORSAllowedOrigins: a list of exact origins, or nothing.
func TestCORSAllowedOrigins(t *testing.T) {
	c, err := FromLookup(lookupFrom(valid()))
	if err != nil || len(c.CORSAllowedOrigins) != 0 {
		t.Fatalf("unset: %v, %v", c.CORSAllowedOrigins, err)
	}
	env := valid()
	env["CORS_ALLOWED_ORIGINS"] = " https://app.example.com, http://localhost:5173 ,"
	c, err = FromLookup(lookupFrom(env))
	if err != nil || len(c.CORSAllowedOrigins) != 2 || c.CORSAllowedOrigins[1] != "http://localhost:5173" {
		t.Fatalf("set: %v, %v", c.CORSAllowedOrigins, err)
	}
	for _, bad := range []string{"*", "app.example.com", "https://app.example.com/", "https://app.example.com/x", "ftp://a.example", "https://u@a.example"} {
		env["CORS_ALLOWED_ORIGINS"] = bad
		if _, err := FromLookup(lookupFrom(env)); err == nil || !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}
