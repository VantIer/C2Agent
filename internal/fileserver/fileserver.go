// Package fileserver provides a small, unauthenticated static file server
// that is independent of the C2 listeners. Directories are listed as HTML and
// files are served with range support; every request is confined to the
// configured root directory.
package fileserver

import (
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Options configures the file server.
type Options struct {
	Host   string
	Port   int
	Root   string
	Logger *log.Logger
}

// Server is a running file server.
type Server struct {
	ln     net.Listener
	srv    *http.Server
	root   string
	logger *log.Logger
	sem    chan struct{}
}

// maxConcurrentRequests bounds simultaneously handled HTTP requests. Directory
// entry counts are deliberately not limited.
const maxConcurrentRequests = 20

var (
	errForbidden = errors.New("forbidden")
	errNotFound  = errors.New("not found")
)

// New validates the root directory, binds the listener and starts serving in
// a background goroutine.
func New(o Options) (*Server, error) {
	if o.Port < 1 || o.Port > 65535 {
		return nil, fmt.Errorf("invalid listen port: %d", o.Port)
	}
	if strings.TrimSpace(o.Root) == "" {
		return nil, errors.New("missing root directory")
	}
	abs, err := filepath.Abs(o.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root is not a directory: %s", real)
	}

	host := o.Host
	if host == "" {
		host = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(o.Port)))
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	logger := o.Logger
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{ln: ln, root: real, logger: logger, sem: make(chan struct{}, maxConcurrentRequests)}
	// Use the handler directly (not ServeMux): ServeMux canonicalizes away
	// ".." segments with a redirect before the traversal check can see them.
	s.srv = &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       20 * time.Second,
	}
	go s.serve()
	return s, nil
}

func (s *Server) serve() {
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.logger.Printf("fileserver: %v", err)
	}
}

// Addr returns the bound listener address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close stops the file server and its active connections.
func (s *Server) Close() error { return s.srv.Close() }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	target, err := s.resolve(r.URL.Path)
	if err != nil {
		switch {
		case errors.Is(err, errForbidden):
			http.Error(w, "Forbidden", http.StatusForbidden)
		case errors.Is(err, errNotFound):
			http.Error(w, "Not Found", http.StatusNotFound)
		default:
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Not Found", http.StatusNotFound)
		} else {
			http.Error(w, "Forbidden", http.StatusForbidden)
		}
		return
	}
	if info.IsDir() {
		s.serveDir(w, r, target)
		return
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	s.serveFile(w, r, target, info)
}

// resolve maps a URL path to a real filesystem path inside the root. It rejects
// ".." segments and any path whose symlink-resolved target escapes the root.
func (s *Server) resolve(urlPath string) (string, error) {
	if strings.ContainsRune(urlPath, 0) {
		return "", errForbidden
	}
	raw := strings.ReplaceAll(urlPath, "\\", "/")
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", errForbidden
		}
	}
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(raw, "/")), "/")
	if rel == "" {
		rel = "."
	}
	local := filepath.FromSlash(rel)
	if filepath.IsAbs(local) {
		return "", errForbidden
	}

	real, err := filepath.EvalSymlinks(filepath.Join(s.root, local))
	if err != nil {
		if os.IsNotExist(err) {
			return "", errNotFound
		}
		return "", errForbidden
	}
	relToRoot, err := filepath.Rel(s.root, real)
	if err != nil || relToRoot == ".." ||
		strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", errForbidden
	}
	return real, nil
}

func (s *Server) serveDir(w http.ResponseWriter, r *http.Request, dir string) {
	if !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	var b strings.Builder
	title := html.EscapeString(r.URL.Path)
	b.WriteString("<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>Index of ")
	b.WriteString(title)
	b.WriteString("</title></head><body><h1>Index of ")
	b.WriteString(title)
	b.WriteString("</h1><ul>\n")
	if r.URL.Path != "/" {
		b.WriteString("<li><a href=\"../\">../</a></li>\n")
	}
	for _, e := range entries {
		name := e.Name()
		href := url.PathEscape(name)
		display := html.EscapeString(name)
		if e.IsDir() {
			href += "/"
			display += "/"
		}
		b.WriteString("<li><a href=\"")
		b.WriteString(href)
		b.WriteString("\">")
		b.WriteString(display)
		b.WriteString("</a></li>\n")
	}
	b.WriteString("</ul></body></html>\n")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, file string, info os.FileInfo) {
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Not Found", http.StatusNotFound)
		} else {
			http.Error(w, "Forbidden", http.StatusForbidden)
		}
		return
	}
	defer f.Close()

	if ct := mime.TypeByExtension(filepath.Ext(file)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
