package webdavserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"inkflow/internal/config"
)

func TestMutationDisabledReturns405AndIsNotAdvertised(t *testing.T) {
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	disabled := newServer(&config.Config{VaultDir: vault}, nil, nil, nil, nil)
	for _, method := range []string{http.MethodDelete, "COPY", "MOVE"} {
		rec := httptest.NewRecorder()
		disabled.ServeHTTP(rec, httptest.NewRequest(method, "/file.txt", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
		if strings.Contains(rec.Header().Get("Allow"), method) {
			t.Errorf("disabled Allow = %q unexpectedly includes %s", rec.Header().Get("Allow"), method)
		}
	}

	enabled := newServer(&config.Config{VaultDir: vault, WebDAV: config.WebDAVConfig{EnableMutation: true}}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	enabled.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/", nil))
	for _, method := range []string{http.MethodDelete, "COPY", "MOVE"} {
		if !strings.Contains(rec.Header().Get("Allow"), method) {
			t.Errorf("enabled Allow = %q does not include %s", rec.Header().Get("Allow"), method)
		}
	}
}

func TestDeleteFileAndCollections(t *testing.T) {
	vault := t.TempDir()
	for name, contents := range map[string]string{
		"file.txt":           "file",
		"tree/child.txt":     "child",
		"tree/nested/one.md": "nested",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(vault, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vault, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(vault, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := mutationServer(vault)
	for _, resource := range []string{"/file.txt", "/empty", "/tree"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, resource, nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s = %d: %s", resource, rec.Code, rec.Body.String())
		}
		if _, err := os.Lstat(filepath.Join(vault, strings.TrimPrefix(resource, "/"))); !os.IsNotExist(err) {
			t.Fatalf("DELETE %s left resource behind: %v", resource, err)
		}
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing = %d, want 404", rec.Code)
	}
}

func TestDeleteCollectionReportsPartialFailureWithoutFollowingSymlink(t *testing.T) {
	vault := t.TempDir()
	partial := filepath.Join(vault, "partial")
	if err := os.Mkdir(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "removable.txt"), []byte("remove me"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(partial, "escape")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	mutationServer(vault).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/partial", nil))
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("DELETE partial = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/xml") || !strings.Contains(rec.Body.String(), "/partial/escape") || !strings.Contains(rec.Body.String(), "403 Forbidden") {
		t.Fatalf("partial DELETE multistatus = %s", rec.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(partial, "removable.txt")); !os.IsNotExist(err) {
		t.Fatalf("removable sibling remained: %v", err)
	}
	if body, err := os.ReadFile(outside); err != nil || string(body) != "private" {
		t.Fatalf("outside symlink target changed: %q, %v", body, err)
	}
}

func TestCopyFileOverwriteDestinationAndCollectionDepth(t *testing.T) {
	vault := t.TempDir()
	for name, contents := range map[string]string{
		"a.txt":             "original",
		"archive/existing":  "keep",
		"collection/child":  "child",
		"collection/deep/x": "deep",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(vault, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vault, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := mutationServer(vault)

	rec := doDestinationRequest(srv, "COPY", "/a.txt", "/archive/new.txt", "")
	if rec.Code != http.StatusCreated || readTestFile(t, filepath.Join(vault, "archive", "new.txt")) != "original" {
		t.Fatalf("new COPY = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doDestinationRequest(srv, "COPY", "/a.txt", "/archive/existing", "F")
	if rec.Code != http.StatusPreconditionFailed || readTestFile(t, filepath.Join(vault, "archive", "existing")) != "keep" {
		t.Fatalf("overwrite F COPY = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doDestinationRequest(srv, "COPY", "/a.txt", "/archive/existing", "")
	if rec.Code != http.StatusNoContent || readTestFile(t, filepath.Join(vault, "archive", "existing")) != "original" {
		t.Fatalf("overwrite COPY = %d: %s", rec.Code, rec.Body.String())
	}

	rec = doDestinationRequest(srv, "COPY", "/collection", "/zero", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("default collection COPY = %d: %s", rec.Code, rec.Body.String())
	}
	if got := readTestFile(t, filepath.Join(vault, "zero", "deep", "x")); got != "deep" {
		t.Fatalf("infinite COPY omitted nested member: %q", got)
	}

	req := httptest.NewRequest("COPY", "/collection", nil)
	req.Header.Set("Destination", "/shallow")
	req.Header.Set("Depth", "0")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Depth 0 COPY = %d: %s", rec.Code, rec.Body.String())
	}
	if entries, err := os.ReadDir(filepath.Join(vault, "shallow")); err != nil || len(entries) != 0 {
		t.Fatalf("Depth 0 COPY entries = %v, err=%v", entries, err)
	}
}

func TestCopyRejectsCrossServerAndInvalidDestinations(t *testing.T) {
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "a.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := mutationServer(vault)
	for name, destination := range map[string]string{
		"cross server":   "http://other.example/destination",
		"outside path":   "/%2e%2e/outside",
		"missing parent": "/missing/a.txt",
	} {
		t.Run(name, func(t *testing.T) {
			rec := doDestinationRequest(srv, "COPY", "/a.txt", destination, "")
			if name == "cross server" && rec.Code != http.StatusBadGateway {
				t.Fatalf("COPY %s = %d, want 502", name, rec.Code)
			}
			if name != "cross server" && (rec.Code < 400 || rec.Code >= 500) {
				t.Fatalf("COPY %s = %d, want 4xx", name, rec.Code)
			}
		})
	}
}

func TestMoveNewExistingAndDescendant(t *testing.T) {
	vault := t.TempDir()
	for name, contents := range map[string]string{
		"a.txt":          "new",
		"b.txt":          "source",
		"archive/b.txt":  "old",
		"tree/child.txt": "child",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(vault, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vault, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := mutationServer(vault)

	rec := doDestinationRequest(srv, "MOVE", "/a.txt", "/archive/a.txt", "")
	if rec.Code != http.StatusCreated || readTestFile(t, filepath.Join(vault, "archive", "a.txt")) != "new" {
		t.Fatalf("new MOVE = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(vault, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("moved source remains: %v", err)
	}

	rec = doDestinationRequest(srv, "MOVE", "/b.txt", "/archive/b.txt", "F")
	if rec.Code != http.StatusPreconditionFailed || readTestFile(t, filepath.Join(vault, "b.txt")) != "source" {
		t.Fatalf("overwrite F MOVE = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doDestinationRequest(srv, "MOVE", "/b.txt", "/archive/b.txt", "T")
	if rec.Code != http.StatusNoContent || readTestFile(t, filepath.Join(vault, "archive", "b.txt")) != "source" {
		t.Fatalf("overwrite MOVE = %d: %s", rec.Code, rec.Body.String())
	}

	rec = doDestinationRequest(srv, "MOVE", "/tree", "/tree/nested", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("descendant MOVE = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doDestinationRequest(srv, "MOVE", "/tree/child.txt", "/tree", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ancestor MOVE = %d: %s", rec.Code, rec.Body.String())
	}
	if got := readTestFile(t, filepath.Join(vault, "tree", "child.txt")); got != "child" {
		t.Fatalf("overlapping MOVE changed source: %q", got)
	}
}

func mutationServer(vault string) *Server {
	return newServer(&config.Config{VaultDir: vault, WebDAV: config.WebDAVConfig{EnableMutation: true}}, nil, nil, nil, nil)
}

func doDestinationRequest(srv *Server, method, source, destination, overwrite string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, source, nil)
	req.Header.Set("Destination", destination)
	if overwrite != "" {
		req.Header.Set("Overwrite", overwrite)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func readTestFile(t *testing.T, filename string) string {
	t.Helper()
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
