// Command devtoken prints a signed HS256 JWT for local development.
//
// It reads the signing key from JWT_SECRET, the same variable the server uses.
// Never use it to mint tokens for a production deployment.
//
// The tenant must be one the server accepts: it matches
// domain.TenantIDPattern exactly as given and is not "default". So must the
// user: 1 to 128 printable ASCII characters (domain.ValidateUserID), with no
// spaces around it, which the server would trim away.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/rtrtarun/taskify/internal/domain"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv("JWT_SECRET"), time.Now(), os.Stdout, os.Stderr))
}

// run mints the token and returns the exit code: 0 on success, 1 when the
// token cannot be made, 2 for bad flags.
func run(args []string, secret string, now time.Time, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devtoken", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "tenant_dev", "tenant_id claim")
	user := fs.String("user", "usr_dev", "sub claim (the user ID)")
	role := fs.String("role", "admin", "role claim: admin or member")
	ttl := fs.Duration("ttl", 24*time.Hour, "token lifetime")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if secret == "" {
		_, _ = fmt.Fprintln(stderr, "devtoken: JWT_SECRET is not set")
		return 1
	}
	if *role != "admin" && *role != "member" {
		_, _ = fmt.Fprintln(stderr, "devtoken: -role must be admin or member")
		return 2
	}
	if domain.ValidateTenantID(*tenant) != nil {
		_, _ = fmt.Fprintf(stderr, "devtoken: -tenant must match %s and not be %q\n", domain.TenantIDPattern, domain.ReservedTenantID)
		return 2
	}
	if domain.ValidateUserID("-user", *user) != nil || strings.TrimSpace(*user) != *user {
		_, _ = fmt.Fprintf(stderr, "devtoken: -user must be 1 to %d printable ASCII characters, without leading or trailing spaces\n",
			domain.MaxUserIDLen)
		return 2
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":       *user,
		"tenant_id": *tenant,
		"role":      *role,
		"iat":       now.Unix(),
		"exp":       now.Add(*ttl).Unix(),
	})
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "devtoken: could not sign token")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, signed)
	return 0
}
