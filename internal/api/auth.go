package api

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/rtrtarun/taskify/internal/domain"
)

// Roles. A token without a recognised role is a member.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Identity is the authenticated caller. Every tenant-scoped query takes the
// tenant from here, never from the request body.
type Identity struct {
	TenantID string
	UserID   string
	Role     string
}

// IsAdmin reports whether the caller is a workspace admin.
func (i Identity) IsAdmin() bool { return i.Role == RoleAdmin }

type identityKey struct{}

// IdentityFrom returns the caller's identity, set by the auth middleware.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// AuthConfig configures caller authentication (docs/architecture.md section
// 6.4, decision 2).
type AuthConfig struct {
	// Development accepts the X-Tenant-ID and X-User-ID headers from any
	// address. Never set it in production.
	Development bool
	Algorithm   string // HS256 or RS256
	Secret      string // HS256 key
	PublicKey   string // RS256 key, PEM
	// TrustedGatewayCIDRs may send identity headers instead of a token. The
	// check uses the TCP peer address, never X-Forwarded-For.
	TrustedGatewayCIDRs []netip.Prefix
}

// Identity headers, accepted only in development or from a trusted gateway.
const (
	HeaderTenantID = "X-Tenant-ID"
	HeaderUserID   = "X-User-ID"
	HeaderUserRole = "X-User-Role"
)

type authenticator struct {
	cfg AuthConfig
	key any // []byte, *rsa.PublicKey, or nil when tokens cannot be verified
}

func newAuthenticator(cfg AuthConfig) (*authenticator, error) {
	a := &authenticator{cfg: cfg}
	switch cfg.Algorithm {
	case "", "HS256":
		a.cfg.Algorithm = "HS256"
		if cfg.Secret != "" {
			a.key = []byte(cfg.Secret)
		}
	case "RS256":
		if cfg.PublicKey != "" {
			key, err := jwt.ParseRSAPublicKeyFromPEM([]byte(cfg.PublicKey))
			if err != nil {
				return nil, fmt.Errorf("JWT_PUBLIC_KEY: %w", err)
			}
			a.key = key
		}
	default:
		return nil, fmt.Errorf("unsupported JWT algorithm %q", cfg.Algorithm)
	}
	return a, nil
}

type claims struct {
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// middleware authenticates the caller and puts the Identity in the context.
// A request that presents a token is judged on the token alone: a bad token
// never falls back to the identity headers.
func (a *authenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := a.authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="taskify"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: CodeUnauthorized, Message: err.Error()}})
			return
		}
		if f := fieldsFrom(r.Context()); f != nil {
			f.tenantID, f.userID = id.TenantID, id.UserID
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func (a *authenticator) authenticate(r *http.Request) (Identity, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		token, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || strings.TrimSpace(token) == "" {
			return Identity{}, errors.New("the Authorization header must be: Bearer <token>")
		}
		return a.verify(strings.TrimSpace(token))
	}
	tenant, user := r.Header.Get(HeaderTenantID), r.Header.Get(HeaderUserID)
	if tenant == "" && user == "" {
		return Identity{}, errors.New("authentication required")
	}
	if !a.cfg.Development && !a.fromTrustedGateway(r) {
		return Identity{}, errors.New("identity headers are accepted only from a trusted gateway; send a bearer token")
	}
	return newIdentity(tenant, user, r.Header.Get(HeaderUserRole))
}

func (a *authenticator) verify(token string) (Identity, error) {
	if a.key == nil {
		return Identity{}, errors.New("token verification is not configured on this server")
	}
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) {
		if k, ok := a.key.(*rsa.PublicKey); ok {
			return k, nil
		}
		return a.key, nil
	},
		jwt.WithValidMethods([]string{a.cfg.Algorithm}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		// The library's message says what was wrong (expired, bad
		// signature) without echoing the token.
		return Identity{}, fmt.Errorf("invalid token: %w", errUnwrapJWT(err))
	}
	return newIdentity(c.TenantID, c.Subject, c.Role)
}

// errUnwrapJWT keeps the most specific reason from a jwt error.
func errUnwrapJWT(err error) error {
	for _, e := range []error{jwt.ErrTokenExpired, jwt.ErrTokenSignatureInvalid, jwt.ErrTokenMalformed,
		jwt.ErrTokenUnverifiable, jwt.ErrTokenRequiredClaimMissing, jwt.ErrTokenNotValidYet} {
		if errors.Is(err, e) {
			return e
		}
	}
	return errors.New("token rejected")
}

// newIdentity checks an identity from a token or the development headers.
// The tenant ID must match domain.TenantIDPattern exactly as given, never
// trimmed and compared case-sensitively, and must not be "default" (AP-04):
// a tenant AgentPulse would refuse is refused here first. The user ID is
// trimmed, as it always was, and must then be 1 to 128 printable ASCII
// characters (AP-11), since it appears in AgentPulse idempotency keys.
func newIdentity(tenant, user, role string) (Identity, error) {
	user = strings.TrimSpace(user)
	if domain.ValidateTenantID(tenant) != nil {
		return Identity{}, fmt.Errorf("the identity must carry a tenant ID matching %s, other than %q",
			domain.TenantIDPattern, domain.ReservedTenantID)
	}
	if domain.ValidateUserID("user ID", user) != nil {
		return Identity{}, fmt.Errorf("the identity must carry a user ID of 1 to %d printable ASCII characters", domain.MaxUserIDLen)
	}
	if role != RoleAdmin {
		role = RoleMember
	}
	return Identity{TenantID: tenant, UserID: user, Role: role}, nil
}

// fromTrustedGateway checks the TCP peer address. X-Forwarded-For is set by
// the caller and proves nothing, so it is never consulted; for the same
// reason the router must not use middleware that rewrites RemoteAddr from it.
func (a *authenticator) fromTrustedGateway(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	addr := ap.Addr().Unmap()
	for _, p := range a.cfg.TrustedGatewayCIDRs {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// requireAdmin rejects callers who are not admins.
func requireAdmin(r *http.Request) error {
	if id, ok := IdentityFrom(r.Context()); ok && id.IsAdmin() {
		return nil
	}
	return newError(http.StatusForbidden, CodeForbidden, "this action requires the admin role")
}
