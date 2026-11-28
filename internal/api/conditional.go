package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rtrtarun/taskify/internal/repository"
)

// List ETags (M14-02). The web client polls the lists with If-None-Match, so
// an unchanged list answers 304 without a body.
//
// The task list's tag comes from a small count-and-latest-update query, so
// an unchanged list is never loaded or serialised. The other lists (agent
// jobs, outbox) have no such query; their tag is a hash of the response body,
// which saves the transfer but not the read.
//
// Tags are weak (W/"...") because they promise the same list, not the same
// bytes.

// listETag is the tag of a task list: it covers the caller's tenant, the
// request's query (sorted, so parameter order does not matter), and the
// list's state. A change to any task a list holds changes the count or the
// latest updated_at; an archive removes a task from the list and so changes
// the count, and for a delta list it changes ArchivedSince as well.
func listETag(tenantID string, query string, st repository.ListState) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00%d", tenantID, query, st.Count, st.Latest.UnixMicro(), st.ArchivedSince)
	return weakTag(h.Sum(nil))
}

// bodyETag is the tag of a list that is identified by its content.
func bodyETag(body []byte) string {
	sum := sha256.Sum256(body)
	return weakTag(sum[:])
}

func weakTag(sum []byte) string { return `W/"` + hex.EncodeToString(sum[:12]) + `"` }

// notModified sets the ETag and, when the request's If-None-Match names it,
// answers 304 and reports true.
func notModified(w http.ResponseWriter, r *http.Request, tag string) bool {
	w.Header().Set("ETag", tag)
	if !ifNoneMatch(r.Header.Get("If-None-Match"), tag) {
		return false
	}
	w.WriteHeader(http.StatusNotModified)
	return true
}

// ifNoneMatch reports whether the header lists tag (weak comparison) or *.
func ifNoneMatch(header, tag string) bool {
	if header == "" {
		return false
	}
	want := strings.TrimPrefix(tag, "W/")
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == want {
			return true
		}
	}
	return false
}

// writeConditionalJSON writes v as JSON with a body-hash ETag, or answers
// 304 when the caller already holds that body.
func writeConditionalJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: CodeInternal, Message: "internal error"}})
		return
	}
	if notModified(w, r, bodyETag(body)) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}
