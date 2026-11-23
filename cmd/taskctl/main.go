// Command taskctl is the Taskify command-line client (docs/architecture.md
// section 9.5). It talks only to the REST API, through pkg/client.
package main

import "os"

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	a := newApp(os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	os.Exit(a.run(os.Args[1:]))
}
