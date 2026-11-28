// Command e2edb makes and removes the throwaway database the browser tests
// (web/e2e) run the server against.
//
//	e2edb create <DATABASE_URL>   creates taskify_e2e_<random>, applies every
//	                              migration, and prints its URL
//	e2edb drop <URL>              drops the database that URL names
//
// The role in DATABASE_URL needs CREATEDB.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rtrtarun/taskify/migrations"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: e2edb create|drop <url>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "create":
		err = create(ctx, os.Args[2])
	case "drop":
		err = drop(ctx, os.Args[2])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2edb:", err)
		os.Exit(1)
	}
}

func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func create(ctx context.Context, base string) error {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	name := "taskify_e2e_" + hex.EncodeToString(b[:])
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return err
	}
	dbURL, err := withDatabase(base, name)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := migrations.Apply(ctx, func(ctx context.Context, sql string) error {
		_, err := conn.Exec(ctx, sql)
		return err
	}, migrations.Up); err != nil {
		return err
	}
	fmt.Println(dbURL)
	return nil
}

func drop(ctx context.Context, dbURL string) error {
	u, err := url.Parse(dbURL)
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !strings.HasPrefix(name, "taskify_e2e_") {
		return fmt.Errorf("refusing to drop %q: not a taskify_e2e_ database", name)
	}
	base, err := withDatabase(dbURL, "postgres")
	if err != nil {
		return err
	}
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(ctx) }()
	_, err = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	return err
}
