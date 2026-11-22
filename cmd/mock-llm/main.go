// Command mock-llm runs a scriptable OpenAI-compatible model server, for the
// conformance suite and local runs without a model. Point Taskify's
// LLM_BASE_URL at http://host:PORT/v1. Scripts are added with
// POST /_mock/scripts (a JSON list of mockllm.Script); requests are listed
// at GET /_mock/requests, and POST /_mock/reset clears both.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/rtrtarun/taskify/tests/mockllm"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9191"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mockllm.New(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("mock LLM listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}
