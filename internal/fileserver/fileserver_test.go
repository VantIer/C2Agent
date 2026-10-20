package fileserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Server{root: real, sem: make(chan struct{}, maxConcurrentRequests)}
	return http.HandlerFunc(s.handle), real
}

func TestDirectoryListing(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"hello.txt", "sub/"} {
		if !strings.Contains(body, want) {
			t.Fatalf("listing missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "href=\"../\"") {
		t.Fatal("root listing should not contain a parent link")
	}
}

func TestDirectoryRedirect(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sub", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/sub/" {
		t.Fatalf("location = %q, want /sub/", loc)
	}
}

func TestFileDownload(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hello.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Body); string(body) != "hello world" {
		t.Fatalf("body = %q", body)
	}
}

func TestMissingFile(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	s := &Server{root: t.TempDir(), sem: make(chan struct{}, 1)}
	s.sem <- struct{}{}
	rec := httptest.NewRecorder()
	http.HandlerFunc(s.handle).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	<-s.sem
}

func TestPathTraversalForbidden(t *testing.T) {
	h, root := newTestHandler(t)
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, target := range []string{
		"/../secret.txt",
		"/%2e%2e/secret.txt",
		"/sub/../../secret.txt",
		`/..\secret.txt`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", target, rec.Code)
		}
	}
}

func TestSymlinkEscapeForbidden(t *testing.T) {
	h, root := newTestHandler(t)
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/link.txt", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
