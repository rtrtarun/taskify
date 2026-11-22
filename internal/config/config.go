// Package config loads and validates Taskify's configuration from the
// environment. See docs/architecture.md section 5.8.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const (
	EnvDevelopment = "development"
	EnvProduction  = "production"

	JWTAlgHS256 = "HS256"
	JWTAlgRS256 = "RS256"
)

// Config is the validated process configuration.
type Config struct {
	Env  string
	Port int

	DatabaseURL   string
	PublicBaseURL string
	// AppBaseURL is the web app's address, without a trailing slash, or
	// empty. Notifications link to {AppBaseURL}/tasks/{id} and
	// {AppBaseURL}/brief as their view action (AP-10).
	AppBaseURL string

	AgentPulseBaseURL       string
	AgentPulseAPIKey        string
	AgentPulseWebhookSecret string

	OutboxWorkers      int
	OutboxBatchSize    int
	OutboxPollInterval time.Duration
	OutboxMaxAttempts  int
	OutboxLease        time.Duration
	// OutboxRetryMaxDelay caps the backoff between attempts to send a row.
	OutboxRetryMaxDelay time.Duration
	SchedulerInterval   time.Duration

	JWTAlgorithm string
	JWTSecret    string
	JWTPublicKey string

	TrustedGatewayCIDRs []netip.Prefix

	RateLimitRPS   float64
	RateLimitBurst int

	// AgentsEnabled is the process-level switch for the agent runtime.
	// Per-workspace control is in agent_settings.
	AgentsEnabled    bool
	AgentWorkers     int
	AgentMaxTurns    int
	AgentMaxAttempts int

	// DigestLoadThreshold is how many in-progress critical tasks make an
	// assignee overloaded: a non-critical assignment then waits for their
	// digest (G14).
	DigestLoadThreshold int
	// DigestBriefHourUTC is the hour of the day, in UTC, from which morning
	// briefs are queued; there is no per-user time zone (G15).
	DigestBriefHourUTC int

	// LLMBaseURL and LLMModel are required when AgentsEnabled is true.
	// LLMAPIKey is empty for local Ollama.
	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string
	LLMTimeout time.Duration
}

// IsDevelopment reports whether the header identity fallback and other
// development conveniences are enabled.
func (c Config) IsDevelopment() bool { return c.Env == EnvDevelopment }

// String never includes secrets, so a Config is safe to log.
func (c Config) String() string {
	return fmt.Sprintf("env=%s port=%d agentpulse=%s jwt_alg=%s outbox_workers=%d agents=%t llm=%s model=%s",
		c.Env, c.Port, c.AgentPulseBaseURL, c.JWTAlgorithm, c.OutboxWorkers, c.AgentsEnabled, c.LLMBaseURL, c.LLMModel)
}

// Load reads configuration from the process environment. When ENV is
// "development" it first loads a .env file if one exists, without overriding
// variables that are already set.
func Load() (Config, error) {
	if os.Getenv("ENV") == EnvDevelopment {
		_ = godotenv.Load() // a missing .env file is not an error
	}
	return FromLookup(os.LookupEnv)
}

// FromLookup builds a Config from the given lookup function. All problems are
// reported together, each naming the variable at fault.
func FromLookup(lookup func(string) (string, bool)) (Config, error) {
	r := &reader{lookup: lookup}

	c := Config{
		Env:  r.oneOf("ENV", EnvProduction, EnvDevelopment, EnvProduction),
		Port: r.intRange("PORT", 8080, 1, 65535),

		DatabaseURL:   r.required("DATABASE_URL"),
		PublicBaseURL: strings.TrimRight(r.requiredURL("PUBLIC_BASE_URL"), "/"),
		AppBaseURL:    strings.TrimRight(r.optionalURL("APP_BASE_URL"), "/"),

		AgentPulseBaseURL:       strings.TrimRight(r.requiredURL("AGENTPULSE_BASE_URL"), "/"),
		AgentPulseAPIKey:        r.required("AGENTPULSE_API_KEY"),
		AgentPulseWebhookSecret: r.required("AGENTPULSE_WEBHOOK_SECRET"),

		OutboxWorkers:       r.intRange("OUTBOX_WORKERS", 8, 1, 1024),
		OutboxBatchSize:     r.intRange("OUTBOX_BATCH_SIZE", 100, 1, 10000),
		OutboxPollInterval:  r.duration("OUTBOX_POLL_INTERVAL", 200*time.Millisecond),
		OutboxMaxAttempts:   r.intRange("OUTBOX_MAX_ATTEMPTS", 10, 1, 1000),
		OutboxLease:         r.duration("OUTBOX_LEASE", 60*time.Second),
		OutboxRetryMaxDelay: r.duration("OUTBOX_RETRY_MAX_DELAY", 60*time.Second),
		SchedulerInterval:   r.duration("SCHEDULER_INTERVAL", time.Minute),

		JWTAlgorithm: r.oneOf("JWT_ALGORITHM", JWTAlgHS256, JWTAlgHS256, JWTAlgRS256),
		JWTSecret:    r.optional("JWT_SECRET"),
		JWTPublicKey: r.optional("JWT_PUBLIC_KEY"),

		TrustedGatewayCIDRs: r.cidrs("TRUSTED_GATEWAY_CIDRS"),

		RateLimitRPS:   r.positiveFloat("RATE_LIMIT_RPS", 20),
		RateLimitBurst: r.intRange("RATE_LIMIT_BURST", 40, 1, 1_000_000),

		AgentsEnabled:    r.boolean("AGENTS_ENABLED", false),
		AgentWorkers:     r.intRange("AGENT_WORKERS", 4, 1, 256),
		AgentMaxTurns:    r.intRange("AGENT_MAX_TURNS", 6, 1, 50),
		AgentMaxAttempts: r.intRange("AGENT_MAX_ATTEMPTS", 3, 1, 20),

		DigestLoadThreshold: r.intRange("DIGEST_LOAD_THRESHOLD", 3, 1, 1000),
		DigestBriefHourUTC:  r.intRange("DIGEST_BRIEF_HOUR_UTC", 8, 0, 23),

		LLMBaseURL: strings.TrimRight(r.optionalURL("LLM_BASE_URL"), "/"),
		LLMAPIKey:  r.optional("LLM_API_KEY"),
		LLMModel:   r.optional("LLM_MODEL"),
		LLMTimeout: r.duration("LLM_TIMEOUT", 30*time.Second),
	}

	// A runtime with no model would fail every job; refuse to start instead.
	if c.AgentsEnabled {
		if c.LLMBaseURL == "" {
			r.fail("LLM_BASE_URL", "is required when AGENTS_ENABLED=true")
		}
		if c.LLMModel == "" {
			r.fail("LLM_MODEL", "is required when AGENTS_ENABLED=true")
		}
	}

	// A verification key is mandatory in production. In development the header
	// fallback can stand in for tokens, so the key is optional.
	if c.Env == EnvProduction {
		switch c.JWTAlgorithm {
		case JWTAlgHS256:
			if c.JWTSecret == "" {
				r.fail("JWT_SECRET", "is required when ENV=production and JWT_ALGORITHM=HS256")
			}
		case JWTAlgRS256:
			if c.JWTPublicKey == "" {
				r.fail("JWT_PUBLIC_KEY", "is required when ENV=production and JWT_ALGORITHM=RS256")
			}
		}
	}

	if len(r.errs) > 0 {
		return Config{}, errors.Join(r.errs...)
	}
	return c, nil
}

type reader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (r *reader) fail(name, msg string) {
	// The offending value is deliberately left out: it may be a secret.
	r.errs = append(r.errs, fmt.Errorf("config: %s %s", name, msg))
}

func (r *reader) optional(name string) string {
	v, _ := r.lookup(name)
	return strings.TrimSpace(v)
}

func (r *reader) required(name string) string {
	v := r.optional(name)
	if v == "" {
		r.fail(name, "is required")
	}
	return v
}

func (r *reader) optionalURL(name string) string {
	v := r.optional(name)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		r.fail(name, "must be an absolute http or https URL")
	}
	return v
}

func (r *reader) boolean(name string, def bool) bool {
	v := r.optional(name)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.fail(name, "must be true or false")
		return def
	}
	return b
}

func (r *reader) requiredURL(name string) string {
	v := r.required(name)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		r.fail(name, "must be an absolute http or https URL")
	}
	return v
}

func (r *reader) oneOf(name, def string, allowed ...string) string {
	v := r.optional(name)
	if v == "" {
		return def
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	r.fail(name, "must be one of "+strings.Join(allowed, ", "))
	return def
}

func (r *reader) intRange(name string, def, lo, hi int) int {
	v := r.optional(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		r.fail(name, fmt.Sprintf("must be an integer between %d and %d", lo, hi))
		return def
	}
	return n
}

func (r *reader) positiveFloat(name string, def float64) float64 {
	v := r.optional(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		r.fail(name, "must be a positive number")
		return def
	}
	return f
}

func (r *reader) duration(name string, def time.Duration) time.Duration {
	v := r.optional(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		r.fail(name, "must be a positive duration such as 200ms or 1m")
		return def
	}
	return d
}

func (r *reader) cidrs(name string) []netip.Prefix {
	v := r.optional(name)
	if v == "" {
		return nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			r.fail(name, "must be a comma-separated list of CIDR ranges such as 10.0.0.0/8")
			return nil
		}
		out = append(out, p.Masked())
	}
	return out
}
