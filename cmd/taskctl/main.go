// Command taskctl is the Taskify command-line client (docs/architecture.md
// section 9.5). It talks only to the REST API, through pkg/client.
package main

import (
	"context"
	"os"
	"os/signal"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	a := newApp(os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	a.ctx = ctx
	code := a.run(os.Args[1:])
	stop()
	os.Exit(code)
}
