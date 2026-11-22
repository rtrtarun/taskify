// Package contract holds Taskify's language-neutral contract
// (docs/architecture.md section 9.2): the OpenAPI document every client is
// generated from, and the files Go code loads from it.
//
// It imports nothing from the server, so the conformance runner and client
// generators can use it without depending on the reference implementation.
package contract

import _ "embed"

// OpenAPI is contract/openapi.yaml: every endpoint, request and response
// body, status code, and the shared error body.
//
//go:embed openapi.yaml
var OpenAPI []byte
