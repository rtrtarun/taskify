// Command mock-agentpulse runs the AgentPulse stand-in as a standalone server,
// for docker compose and for the conformance suite.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9090"
	}

	mock := mockagentpulse.New()

	// Control endpoints, outside the AgentPulse surface, let a test process
	// inspect and script the mock over HTTP.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_mock/requests", func(w http.ResponseWriter, _ *http.Request) {
		type entry struct {
			Method string          `json:"method"`
			Path   string          `json:"path"`
			Query  string          `json:"query,omitempty"`
			Header http.Header     `json:"header"`
			Body   json.RawMessage `json:"body,omitempty"`
		}
		out := []entry{}
		for _, r := range mock.Requests() {
			e := entry{Method: r.Method, Path: r.Path, Query: r.Query, Header: r.Header}
			if json.Valid(r.Body) {
				e.Body = r.Body
			}
			out = append(out, e)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": out})
	})
	mux.HandleFunc("POST /_mock/fail", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			Status int    `json:"status"`
			Count  int    `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Method == "" || in.Path == "" || in.Status < 400 {
			http.Error(w, "need method, path, status (400 or above), count", http.StatusBadRequest)
			return
		}
		mock.FailNext(in.Method, in.Path, in.Status, in.Count)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /_mock/reset", func(w http.ResponseWriter, _ *http.Request) {
		mock.Reset()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", mock)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("mock AgentPulse listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}
