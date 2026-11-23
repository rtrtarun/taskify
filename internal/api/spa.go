package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// spa serves the embedded frontend (M14-12). It takes a request only when the
// request is a read that names a built file, or a page navigation to a
// client-side route (the browser asks for text/html); everything else,
// above all every path under /api/, goes to the API router. So the frontend
// never shadows an API route, and an unknown API path stays a JSON 404.
type spa struct {
	fsys fs.FS
}

// serverPaths are answered by the server and never by the frontend.
var serverPaths = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}

func (s spa) claims(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/") || p == "/api" || serverPaths[p] {
		return false
	}
	if s.file(p) != "" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// file returns the built file a URL path names, or "".
func (s spa) file(urlPath string) string {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" {
		return ""
	}
	if info, err := fs.Stat(s.fsys, name); err == nil && !info.IsDir() {
		return name
	}
	return ""
}

func (s spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := s.file(r.URL.Path)
	if name == "" {
		name = "index.html" // a client-side route
	}
	f, err := s.fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rs, ok := f.(readSeeker)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // file names carry a hash
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, info.ModTime().Truncate(time.Second), rs)
}

type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}

// withSPA lets the frontend answer what it claims and the API the rest.
func withSPA(api http.Handler, fsys fs.FS) http.Handler {
	s := spa{fsys: fsys}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.claims(r) {
			s.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}
