package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// HeaderRequestID carries the request ID in both directions.
const HeaderRequestID = "X-Request-ID"

// logFields collects values that inner handlers learn (the caller's tenant)
// for the access log written by the outer middleware.
type logFields struct {
	requestID string
	tenantID  string
	userID    string
}

type fieldsKey struct{}

func fieldsFrom(ctx context.Context) *logFields {
	f, _ := ctx.Value(fieldsKey{}).(*logFields)
	return f
}

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestContext assigns a request ID (reusing a well-formed incoming one)
// and writes one access log line per request.
func requestContext(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if !safeRequestID.MatchString(id) {
				id = uuid.NewString()
			}
			w.Header().Set(HeaderRequestID, id)
			f := &logFields{requestID: id}
			ctx := context.WithValue(r.Context(), fieldsKey{}, f)

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r.WithContext(ctx))

			route := ""
			if rc := chi.RouteContext(ctx); rc != nil {
				route = rc.RoutePattern()
			}
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []any{
				"request_id", id, "method", r.Method, "route", route, "status", status,
				"duration_ms", time.Since(start).Milliseconds(),
			}
			if f.tenantID != "" {
				attrs = append(attrs, "tenant_id", f.tenantID, "user_id", f.userID)
			}
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			}
			logger.Log(ctx, level, "http request", attrs...)
		})
	}
}

// recoverer turns a panic into a 500 response and a logged stack trace.
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p) // the server's signal to drop the connection
				}
				logger.ErrorContext(r.Context(), "panic serving request",
					"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: CodeInternal, Message: "internal error"}})
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimitConfig is a token bucket per tenant and user (default G5).
type RateLimitConfig struct {
	RPS   float64
	Burst int
}

// rateLimiter keeps one bucket per caller, in memory. Each replica limits
// independently, so the effective limit scales with the replica count.
type rateLimiter struct {
	mu        sync.Mutex
	limit     rate.Limit
	burst     int
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newRateLimiter(cfg RateLimitConfig) *rateLimiter {
	if cfg.RPS <= 0 {
		cfg.RPS = 20
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 40
	}
	return &rateLimiter{limit: rate.Limit(cfg.RPS), burst: cfg.Burst, buckets: map[string]*bucket{}, lastSweep: time.Now()}
}

const idleBucket = 10 * time.Minute

func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if now.Sub(rl.lastSweep) > time.Minute {
		for k, b := range rl.buckets {
			if now.Sub(b.seen) > idleBucket {
				delete(rl.buckets, k)
			}
		}
		rl.lastSweep = now
	}
	b := rl.buckets[key]
	if b == nil {
		b = &bucket{lim: rate.NewLimiter(rl.limit, rl.burst)}
		rl.buckets[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// middleware must run after authentication: it keys on the identity.
func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := IdentityFrom(r.Context())
		if !rl.allow(id.TenantID + "\x00" + id.UserID) {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Code: CodeRateLimited, Message: "too many requests; slow down"}})
			return
		}
		next.ServeHTTP(w, r)
	})
}
