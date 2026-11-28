package api

import (
	"net/http"
	"strings"
)

// corsMiddleware lets the browser origins in allowed call the API from a
// frontend served elsewhere (M14-02). Callers authenticate with a bearer
// token, never a cookie, so no credentials are allowed and a request from
// any other origin gains nothing.
//
// A preflight (OPTIONS with Access-Control-Request-Method) from an origin not
// in the list is refused with 403 and carries no CORS headers. An actual
// request from another origin is served as before but without CORS headers,
// so the browser withholds the answer from the page. Requests with no Origin
// header (curl, the CLI) are untouched. With an empty list the middleware does
// nothing: same-origin use needs no CORS.
func corsMiddleware(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[o] = true
	}
	return func(next http.Handler) http.Handler {
		if len(set) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Add("Vary", "Origin")
			ok := set[origin]
			preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
			switch {
			case preflight && !ok:
				writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{Code: CodeForbidden, Message: "this origin is not allowed"}})
			case preflight:
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", allowedHeaders(r.Header.Get("Access-Control-Request-Headers")))
				h.Set("Access-Control-Max-Age", "600")
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				w.WriteHeader(http.StatusNoContent)
			default:
				if ok {
					h := w.Header()
					h.Set("Access-Control-Allow-Origin", origin)
					h.Set("Access-Control-Expose-Headers", "ETag, Location, Retry-After, X-Request-ID")
				}
				next.ServeHTTP(w, r)
			}
		})
	}
}

// corsHeaders are the request headers a browser client may send.
var corsHeaders = map[string]string{
	"authorization": "Authorization", "content-type": "Content-Type", "if-match": "If-Match",
	"if-none-match": "If-None-Match", "x-request-id": "X-Request-ID",
}

// allowedHeaders echoes the requested headers the API accepts, so a
// preflight naming anything else is answered without it and the browser
// refuses the real request.
func allowedHeaders(requested string) string {
	var out []string
	for _, h := range strings.Split(requested, ",") {
		if canon, ok := corsHeaders[strings.ToLower(strings.TrimSpace(h))]; ok {
			out = append(out, canon)
		}
	}
	return strings.Join(out, ", ")
}
