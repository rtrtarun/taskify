// Command devtoken prints a signed HS256 JWT for local development.
//
// It reads the signing key from JWT_SECRET, the same variable the server uses.
// Never use it to mint tokens for a production deployment.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	tenant := flag.String("tenant", "tenant_dev", "tenant_id claim")
	user := flag.String("user", "usr_dev", "sub claim (the user ID)")
	role := flag.String("role", "admin", "role claim: admin or member")
	ttl := flag.Duration("ttl", 24*time.Hour, "token lifetime")
	flag.Parse()

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "devtoken: JWT_SECRET is not set")
		os.Exit(1)
	}
	if *role != "admin" && *role != "member" {
		fmt.Fprintln(os.Stderr, "devtoken: -role must be admin or member")
		os.Exit(2)
	}

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":       *user,
		"tenant_id": *tenant,
		"role":      *role,
		"iat":       now.Unix(),
		"exp":       now.Add(*ttl).Unix(),
	})
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		fmt.Fprintln(os.Stderr, "devtoken: could not sign token")
		os.Exit(1)
	}
	fmt.Println(signed)
}
