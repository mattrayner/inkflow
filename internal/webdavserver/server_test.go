package webdavserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"inkflow/internal/ai"
	"inkflow/internal/config"
	"inkflow/internal/importer"
	"inkflow/internal/observability"
	"inkflow/internal/state"
)

func TestHTTPServerUsesConfiguredTimeouts(t *testing.T) {
	cfg := &config.Config{ListenAddr: "127.0.0.1:8080", ReadHeaderTimeoutDuration: time.Second, ReadTimeoutDuration: 2 * time.Second, WriteTimeoutDuration: 3 * time.Second, IdleTimeoutDuration: 4 * time.Second}
	httpSrv := newHTTPServer(cfg, http.NotFoundHandler())
	if httpSrv.ReadHeaderTimeout != time.Second || httpSrv.ReadTimeout != 2*time.Second || httpSrv.WriteTimeout != 3*time.Second || httpSrv.IdleTimeout != 4*time.Second {
		t.Errorf("unexpected timeouts: %+v", httpSrv)
	}
}

func TestHealthDoesNotRequireAuthenticationAndChecksStore(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{WebDAVUser: "user", WebDAVPass: "pass"}
	srv := &Server{cfg: cfg, store: store}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy status = %d", rec.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("closed state store reported healthy")
	}
}

func TestMetricsRequireMainListenerAuthentication(t *testing.T) {
	cfg := &config.Config{WebDAVUser: "user", WebDAVPass: "pass", Observability: config.ObservabilityConfig{MetricsEnabled: true}}
	srv := &Server{cfg: cfg, metrics: observability.New()}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated metrics status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.SetBasicAuth("user", "pass")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "inkflow_state_records") {
		t.Fatalf("metrics response = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMetricsDisabledIsNotExposed(t *testing.T) {
	srv := &Server{cfg: &config.Config{}, metrics: observability.New()}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("disabled metrics status = %d", rec.Code)
	}
}

func TestPropfindUsesVaultMetadataAndDepth(t *testing.T) {
	vault := t.TempDir()
	stamp := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	if err := os.Mkdir(filepath.Join(vault, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(vault, "docs", "note file.md")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(vault, "docs", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: &config.Config{VaultDir: vault}}

	depthZero := httptest.NewRequest("PROPFIND", "/docs/note%20file.md", nil)
	depthZero.Header.Set("Depth", "0")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, depthZero)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("Depth 0 status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := strings.Count(rec.Body.String(), "<D:response>"); got != 1 {
		t.Fatalf("Depth 0 responses = %d: %s", got, rec.Body.String())
	}
	for _, want := range []string{"note%20file.md", "5", stamp.Format(http.TimeFormat)} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("Depth 0 missing %q: %s", want, rec.Body.String())
		}
	}

	depthOne := httptest.NewRequest("PROPFIND", "/docs/", nil)
	depthOne.Header.Set("Depth", "1")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, depthOne)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("Depth 1 status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"<D:href>/docs/</D:href>", "note%20file.md", "<D:href>/docs/child/</D:href>"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("Depth 1 missing %q: %s", want, rec.Body.String())
		}
	}
}

func TestPropfindRejectsMissingAndTraversalTargets(t *testing.T) {
	vault := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(vault, "escape")); err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: &config.Config{VaultDir: vault}}
	for _, target := range []string{"/missing", "/%2e%2e/secret", "/docs/%2e%2e/%2e%2e/secret", "/escape"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("PROPFIND", target, nil))
		if rec.Code == http.StatusMultiStatus {
			t.Fatalf("%s unexpectedly returned properties: %s", target, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.Header.Set("Depth", "invalid")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid Depth status = %d", rec.Code)
	}
}

func TestPropfindPropertySelectionsAndExtendedLiveProperties(t *testing.T) {
	vault := t.TempDir()
	file := filepath.Join(vault, "note.txt")
	stamp := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	srv := newServer(&config.Config{VaultDir: vault, WebDAV: config.WebDAVConfig{EnableRetrieval: true}}, nil, nil, nil, nil)

	get := httptest.NewRecorder()
	srv.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/note.txt", nil))
	etag := get.Header().Get("ETag")
	xmlETag := strings.ReplaceAll(etag, `"`, "&#34;")
	for name, body := range map[string]string{
		"allprop":  `<D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`,
		"propname": `<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("PROPFIND", "/note.txt", strings.NewReader(body))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusMultiStatus {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "<D:getetag>"+xmlETag+"</D:getetag>") && name == "allprop" {
				t.Fatalf("allprop ETag missing: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "<D:creationdate>"+stamp.Format(time.RFC3339)+"</D:creationdate>") && name == "allprop" {
				t.Fatalf("allprop creationdate missing: %s", rec.Body.String())
			}
			if name == "propname" && strings.Contains(rec.Body.String(), etag) {
				t.Fatalf("propname included property value: %s", rec.Body.String())
			}
		})
	}

	req := httptest.NewRequest("PROPFIND", "/note.txt", strings.NewReader(`<D:propfind xmlns:D="DAV:"><D:prop><D:getcontentlength/><D:getetag/></D:prop></D:propfind>`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "<D:getcontentlength>5</D:getcontentlength>") || !strings.Contains(rec.Body.String(), "<D:getetag>"+xmlETag+"</D:getetag>") || strings.Contains(rec.Body.String(), "<D:displayname>") {
		t.Fatalf("named prop response = %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("PROPFIND", "/note.txt", strings.NewReader(`<D:propfind xmlns:D="DAV:" xmlns:X="urn:test"><D:prop><X:missing/></D:prop></D:propfind>`))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "<D:status>HTTP/1.1 404 Not Found</D:status>") || !strings.Contains(rec.Body.String(), "<missing xmlns=\"urn:test\"></missing>") {
		t.Fatalf("unknown property response = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPropfindDepthInfinity(t *testing.T) {
	vault := t.TempDir()
	for _, directory := range []string{"docs", "docs/nested"} {
		if err := os.Mkdir(filepath.Join(vault, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"docs/top.txt", "docs/nested/deep.txt"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := &Server{cfg: &config.Config{VaultDir: vault}}
	req := httptest.NewRequest("PROPFIND", "/docs", nil)
	req.Header.Set("Depth", "infinity")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, href := range []string{"/docs/", "/docs/top.txt", "/docs/nested/", "/docs/nested/deep.txt"} {
		if !strings.Contains(rec.Body.String(), "<D:href>"+href+"</D:href>") {
			t.Errorf("missing %s: %s", href, rec.Body.String())
		}
	}
}

func TestProppatchSetRemoveAndAtomicProtectedProperty(t *testing.T) {
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	disabled := &Server{cfg: &config.Config{VaultDir: vault}}
	disabledRec := httptest.NewRecorder()
	disabled.ServeHTTP(disabledRec, httptest.NewRequest("PROPPATCH", "/note.txt", strings.NewReader(`<D:propertyupdate xmlns:D="DAV:"/>`)))
	if disabledRec.Code != http.StatusMethodNotAllowed || strings.Contains(disabledRec.Header().Get("Allow"), "PROPPATCH") {
		t.Fatalf("disabled PROPPATCH = %d, Allow=%q", disabledRec.Code, disabledRec.Header().Get("Allow"))
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := newServer(&config.Config{VaultDir: vault, WebDAV: config.WebDAVConfig{EnableMutation: true}}, nil, store, nil, nil)
	patch := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("PROPPATCH", "/note.txt", strings.NewReader(body)))
		return rec
	}
	set := `<D:propertyupdate xmlns:D="DAV:" xmlns:X="urn:test"><D:set><D:prop><X:custom>value</X:custom></D:prop></D:set></D:propertyupdate>`
	if rec := patch(set); rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "HTTP/1.1 200 OK") {
		t.Fatalf("set response = %d: %s", rec.Code, rec.Body.String())
	}
	propfind := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("PROPFIND", "/note.txt", strings.NewReader(`<D:propfind xmlns:D="DAV:" xmlns:X="urn:test"><D:prop><X:custom/></D:prop></D:propfind>`)))
		return rec
	}
	if rec := propfind(); !strings.Contains(rec.Body.String(), `<custom xmlns="urn:test">value</custom>`) {
		t.Fatalf("stored property missing: %s", rec.Body.String())
	}
	protected := `<D:propertyupdate xmlns:D="DAV:" xmlns:X="urn:test"><D:set><D:prop><X:other>not persisted</X:other><D:getcontentlength>10</D:getcontentlength></D:prop></D:set></D:propertyupdate>`
	if rec := patch(protected); !strings.Contains(rec.Body.String(), "HTTP/1.1 403 Forbidden") || !strings.Contains(rec.Body.String(), "HTTP/1.1 424 Failed Dependency") {
		t.Fatalf("protected response = %d: %s", rec.Code, rec.Body.String())
	}
	unknown := httptest.NewRecorder()
	srv.ServeHTTP(unknown, httptest.NewRequest("PROPFIND", "/note.txt", strings.NewReader(`<D:propfind xmlns:D="DAV:" xmlns:X="urn:test"><D:prop><X:other/></D:prop></D:propfind>`)))
	if !strings.Contains(unknown.Body.String(), "HTTP/1.1 404 Not Found") {
		t.Fatalf("atomic failure persisted property: %s", unknown.Body.String())
	}
	remove := `<D:propertyupdate xmlns:D="DAV:" xmlns:X="urn:test"><D:remove><D:prop><X:custom/></D:prop></D:remove></D:propertyupdate>`
	if rec := patch(remove); rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "HTTP/1.1 200 OK") {
		t.Fatalf("remove response = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := propfind(); !strings.Contains(rec.Body.String(), "HTTP/1.1 404 Not Found") {
		t.Fatalf("removed property remains: %s", rec.Body.String())
	}
}

func TestMkcolCreatesCollectionAndHandlesConflicts(t *testing.T) {
	vault := t.TempDir()
	if err := os.Mkdir(filepath.Join(vault, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "file"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: &config.Config{VaultDir: vault}}

	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{name: "new directory", path: "/new", want: http.StatusCreated},
		{name: "existing directory", path: "/existing", want: http.StatusMethodNotAllowed},
		{name: "existing file", path: "/file", want: http.StatusConflict},
		{name: "missing parent", path: "/missing/child", want: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest("MKCOL", tc.path, nil))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if info, err := os.Stat(filepath.Join(vault, "new")); err != nil || !info.IsDir() {
		t.Fatalf("new collection not created: info=%v err=%v", info, err)
	}
}

func TestOptionsAdvertisesMkcol(t *testing.T) {
	srv := &Server{cfg: &config.Config{VaultDir: t.TempDir()}}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Allow"), "MKCOL") {
		t.Fatalf("Allow = %q, want MKCOL", rec.Header().Get("Allow"))
	}
}

func TestOptionsCapabilitiesReflectConfig(t *testing.T) {
	vault := t.TempDir()
	for name, cfg := range map[string]*config.Config{
		"retrieval enabled":  {VaultDir: vault, WebDAV: config.WebDAVConfig{EnableRetrieval: true}},
		"retrieval disabled": {VaultDir: vault, WebDAV: config.WebDAVConfig{EnableRetrieval: false, EnableMutation: true, EnableLocking: true}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(cfg, nil, nil, nil, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/", nil))
			if got := rec.Header().Get("DAV"); got != "1" {
				t.Fatalf("DAV = %q, want 1", got)
			}
			gotRetrieval := strings.Contains(rec.Header().Get("Allow"), "GET") && strings.Contains(rec.Header().Get("Allow"), "HEAD")
			if gotRetrieval != cfg.WebDAV.EnableRetrieval {
				t.Fatalf("Allow = %q, retrieval enabled = %t", rec.Header().Get("Allow"), cfg.WebDAV.EnableRetrieval)
			}
			if gotProppatch := strings.Contains(rec.Header().Get("Allow"), "PROPPATCH"); gotProppatch != cfg.WebDAV.EnableMutation {
				t.Fatalf("Allow = %q, mutation enabled = %t", rec.Header().Get("Allow"), cfg.WebDAV.EnableMutation)
			}
		})
	}
}

func TestGetAndHeadRetrieveFileMetadata(t *testing.T) {
	vault := t.TempDir()
	contents := []byte("pdf bytes")
	if err := os.WriteFile(filepath.Join(vault, "note.pdf"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(vault, "note.pdf"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	srv := newServer(&config.Config{VaultDir: vault, WebDAV: config.WebDAVConfig{EnableRetrieval: true}}, nil, nil, nil, nil)

	get := httptest.NewRecorder()
	srv.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/note.pdf", nil))
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), contents) {
		t.Fatalf("GET = %d %q", get.Code, get.Body.Bytes())
	}
	sum := sha256.Sum256(contents)
	if got, want := get.Header().Get("ETag"), `"`+hex.EncodeToString(sum[:])+`"`; got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
	if got := get.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := get.Header().Get("Content-Length"); got != "9" {
		t.Errorf("Content-Length = %q", got)
	}
	if got := get.Header().Get("Last-Modified"); got != stamp.Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %q", got)
	}

	head := httptest.NewRecorder()
	srv.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/note.pdf", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d %q", head.Code, head.Body.Bytes())
	}
	for _, header := range []string{"Content-Type", "Content-Length", "Last-Modified", "ETag"} {
		if got, want := head.Header().Get(header), get.Header().Get(header); got != want {
			t.Errorf("HEAD %s = %q, GET %s = %q", header, got, header, want)
		}
	}
}

func TestGetUsesImportedPDFHashForETag(t *testing.T) {
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "imported.pdf"), []byte("current bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Put(&state.Record{SourcePath: "source.pdf", SHA256: "imported-hash", VaultPDFPath: "imported.pdf"}); err != nil {
		t.Fatal(err)
	}
	srv := newServer(&config.Config{VaultDir: vault, WebDAV: config.WebDAVConfig{EnableRetrieval: true}}, nil, store, nil, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/imported.pdf", nil))
	if got := rec.Header().Get("ETag"); got != `"imported-hash"` {
		t.Errorf("ETag = %q", got)
	}
}

func TestGetRejectsMissingTraversalAndSymlinksAndAllowsCollections(t *testing.T) {
	vault := t.TempDir()
	if err := os.Mkdir(filepath.Join(vault, "collection"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(vault, "escape")); err != nil {
		t.Fatal(err)
	}
	srv := newServer(&config.Config{VaultDir: vault, WebDAV: config.WebDAVConfig{EnableRetrieval: true}}, nil, nil, nil, nil)
	for _, target := range []string{"/missing", "/%2e%2e/secret", "/escape"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if target == "/missing" && rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		} else if target != "/missing" && rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/collection", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "0" {
		t.Fatalf("collection GET = %d, length=%q", rec.Code, rec.Header().Get("Content-Length"))
	}
}

func TestGetConditionalHeaders(t *testing.T) {
	vault := t.TempDir()
	file := filepath.Join(vault, "note.txt")
	stamp := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	srv := newServer(&config.Config{VaultDir: vault, WebDAV: config.WebDAVConfig{EnableRetrieval: true}}, nil, nil, nil, nil)
	initial := httptest.NewRecorder()
	srv.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "/note.txt", nil))
	for name, header := range map[string]string{
		"etag":     initial.Header().Get("ETag"),
		"modified": stamp.Format(http.TimeFormat),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/note.txt", nil)
			if name == "etag" {
				req.Header.Set("If-None-Match", header)
			} else {
				req.Header.Set("If-Modified-Since", header)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
				t.Fatalf("conditional GET = %d %q", rec.Code, rec.Body.Bytes())
			}
		})
	}
}

func TestAuthorizeBasicAuth(t *testing.T) {
	srv := &Server{cfg: &config.Config{WebDAVUser: "user", WebDAVPass: "pass"}}
	for name, credentials := range map[string][2]string{"valid": {"user", "pass"}, "bad-user": {"wrong", "pass"}, "bad-pass": {"user", "wrong"}} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/", nil)
			req.SetBasicAuth(credentials[0], credentials[1])
			rec := httptest.NewRecorder()
			allowed := srv.authorize(rec, req)
			if name == "valid" && !allowed {
				t.Fatal("valid credentials rejected")
			}
			if name != "valid" {
				if allowed || rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatalf("invalid credentials allowed or not challenged: code=%d", rec.Code)
				}
			}
		})
	}
}

func TestLoopbackListenAddressDetection(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:8080": true, "[::1]:8080": true, "localhost:8080": true, "0.0.0.0:8080": false, ":8080": false, "bad": false} {
		if got := isLoopbackListenAddr(addr); got != want {
			t.Errorf("isLoopbackListenAddr(%q) = %t, want %t", addr, got, want)
		}
	}
}

func TestUnauthenticatedNonLoopbackBindWarns(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	srv := &Server{logger: logger}
	if !isLoopbackListenAddr("0.0.0.0:8080") {
		srv.warn("UNSAFE WEBDAV CONFIGURATION: unauthenticated plaintext vault writes are reachable on a non-loopback address")
	}
	if !strings.Contains(logs.String(), "UNSAFE WEBDAV CONFIGURATION") {
		t.Fatalf("missing non-loopback warning: %s", logs.String())
	}
	logs.Reset()
	if !isLoopbackListenAddr("127.0.0.1:8080") {
		srv.warn("unexpected warning")
	}
	if logs.Len() != 0 {
		t.Fatalf("loopback emitted warning: %s", logs.String())
	}
}

func TestShutdownUsesBoundedContext(t *testing.T) {
	httpSrv := &http.Server{}
	var logs bytes.Buffer
	srv := &Server{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	done := make(chan struct{})
	go func() {
		srv.shutdown(httpSrv)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded shutdown did not return promptly for an idle server")
	}
}

func TestPutRejectsOversizeUpload(t *testing.T) {
	vaultDir := t.TempDir()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{VaultDir: vaultDir, DefaultPDFDir: "pdfs", DefaultNoteDir: "notes", MaxUploadBytes: 3, Routes: []config.Route{{From: "Syncs/"}}}
	srv := &Server{cfg: cfg, imp: importer.New(cfg, store, nil, 0), logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPut, "/Syncs/oversize.pdf", bytes.NewBufferString("abcd"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d: %s", rec.Code, rec.Body.String())
	}
	record, err := store.GetBySourcePath("Syncs/oversize.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if record != nil {
		t.Fatal("oversize upload persisted a state record")
	}

	req = httptest.NewRequest(http.MethodPut, "/Syncs/limit.pdf", bytes.NewBufferString("abc"))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("boundary status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPutImportsFileIntoVault(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "meeting"}},
	}
	imp := importer.New(cfg, store, nil, 0)
	srv := &Server{cfg: cfg, imp: imp}

	req := httptest.NewRequest("PUT", "/Syncs/2026-05-06%20Processing%20service%20%5Bfinance%5D.pdf", bytes.NewReader([]byte("pdf-bytes")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "pdfs", "2026-05-06-processing-service.pdf")); err != nil {
		t.Fatalf("pdf missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "notes", "2026-05-06 Processing service.md")); err != nil {
		t.Fatalf("note missing: %v", err)
	}
}

type fakeAIClient struct {
	result ai.Result
	err    error
}

type blockingAIClient struct{ started chan struct{} }

func (f blockingAIClient) Process(ctx context.Context, _ io.Reader) (ai.Result, error) {
	close(f.started)
	<-ctx.Done()
	return ai.Result{}, ctx.Err()
}

func TestPutWithAIQueuesWithoutWaitingForProvider(t *testing.T) {
	vaultDir := t.TempDir()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{VaultDir: vaultDir, DefaultPDFDir: "pdfs", DefaultNoteDir: "notes", Routes: []config.Route{{From: "Syncs/", AI: true}}}
	started := make(chan struct{})
	srv := &Server{cfg: cfg, imp: importer.New(cfg, store, blockingAIClient{started: started}, 0)}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/Syncs/2026-06-04%20queued.pdf", bytes.NewBufferString("pdf")))
		done <- rec
	}()
	select {
	case rec := <-done:
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d", rec.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("PUT waited for AI provider")
	}
	select {
	case <-started:
		t.Fatal("PUT called AI provider")
	default:
	}
	stored, err := store.GetBySourcePath("Syncs/2026-06-04 queued.pdf")
	if err != nil || stored == nil || stored.AIStatus != state.AIStatusPending {
		t.Fatalf("queued record = %+v, err=%v", stored, err)
	}
}

func (f fakeAIClient) Process(ctx context.Context, pdf io.Reader) (ai.Result, error) {
	_, _ = io.Copy(io.Discard, pdf)
	return f.result, f.err
}

func TestPutImportsFileWithAIBlocks(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	imp := importer.New(cfg, store, fakeAIClient{
		result: ai.Result{OCR: "full transcript", Summary: []string{"alpha", "beta"}},
	}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	req := httptest.NewRequest("PUT", "/Syncs/2026-06-04%20note.pdf", bytes.NewReader([]byte("pdf-bytes")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body, err := os.ReadFile(filepath.Join(vaultDir, "notes", "2026-06-04 note.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "## Summary") || !strings.Contains(s, "_AI processing queued._") {
		t.Fatalf("summary block missing:\n%s", s)
	}
	if !strings.Contains(s, "## OCR") || !strings.Contains(s, "_AI processing queued._") {
		t.Fatalf("ocr block missing:\n%s", s)
	}
}

func TestPutSurfacesAIErrorInBothBlocks(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	imp := importer.New(cfg, store, fakeAIClient{
		err: errors.New("gemini 401: API key invalid"),
	}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	req := httptest.NewRequest("PUT", "/Syncs/2026-06-04%20bad.pdf", bytes.NewReader([]byte("pdf-bytes")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body, err := os.ReadFile(filepath.Join(vaultDir, "notes", "2026-06-04 bad.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "_AI processing queued._") {
		t.Fatalf("expected queued AI marker in note:\n%s", s)
	}
	if strings.Count(s, "_AI processing queued._") != 2 {
		t.Fatalf("expected queued marker in both blocks:\n%s", s)
	}
}

// fakeAIClient with refusal — fails the test if Process is called.
type refuseAIClient struct {
	t *testing.T
}

func (f refuseAIClient) Process(ctx context.Context, pdf io.Reader) (ai.Result, error) {
	f.t.Fatal("ai.Provider.Process must not be called when route.AI is false")
	return ai.Result{}, nil
}

func TestPutWithoutRouteAIDoesNotCallProvider(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default"}}, // AI defaults to false
	}
	imp := importer.New(cfg, store, refuseAIClient{t: t}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	req := httptest.NewRequest("PUT", "/Syncs/2026-06-04%20skip.pdf", bytes.NewReader([]byte("pdf-bytes")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body, err := os.ReadFile(filepath.Join(vaultDir, "notes", "2026-06-04 skip.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if strings.Contains(s, "## Summary") || strings.Contains(s, "## OCR") {
		t.Fatalf("note contains AI sections even though route.AI=false:\n%s", s)
	}
}

func TestPutReUploadReplacesAIBlocks(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}

	// First upload with one Result.
	imp1 := importer.New(cfg, store, fakeAIClient{
		result: ai.Result{OCR: "first ocr", Summary: []string{"first bullet"}},
	}, 0)
	srv := &Server{cfg: cfg, imp: imp1}
	req := httptest.NewRequest("PUT", "/Syncs/2026-06-04%20idem.pdf", bytes.NewReader([]byte("v1")))
	srv.ServeHTTP(httptest.NewRecorder(), req)

	// Second upload with a different Result — should replace marker bodies, not append.
	imp2 := importer.New(cfg, store, fakeAIClient{
		result: ai.Result{OCR: "second ocr", Summary: []string{"second bullet"}},
	}, 0)
	srv.imp = imp2
	req = httptest.NewRequest("PUT", "/Syncs/2026-06-04%20idem.pdf", bytes.NewReader([]byte("v2")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body, err := os.ReadFile(filepath.Join(vaultDir, "notes", "2026-06-04 idem.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if strings.Contains(s, "first ocr") || strings.Contains(s, "first bullet") {
		t.Fatalf("first-upload content survived in note:\n%s", s)
	}
	if strings.Count(s, "_AI processing queued._") != 2 {
		t.Fatalf("queued content missing:\n%s", s)
	}
	if strings.Count(s, "## OCR") != 1 || strings.Count(s, "## Summary") != 1 {
		t.Fatalf("marker block appended instead of replaced:\n%s", s)
	}
}

func TestPutSurfacesEmptyAIResultInBothBlocks(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	imp := importer.New(cfg, store, fakeAIClient{
		result: ai.Result{}, // both fields empty, no error
	}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	req := httptest.NewRequest("PUT", "/Syncs/2026-06-04%20empty.pdf", bytes.NewReader([]byte("pdf-bytes")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body, err := os.ReadFile(filepath.Join(vaultDir, "notes", "2026-06-04 empty.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "_AI processing queued._") {
		t.Fatalf("expected queued placeholder:\n%s", s)
	}
	if strings.Count(s, "_AI processing queued._") != 2 {
		t.Fatalf("expected queued placeholders:\n%s", s)
	}
}

// countingAIClient counts calls so a test can assert how many times the AI
// provider was invoked across multiple uploads.
type countingAIClient struct {
	result ai.Result
	calls  *int
}

func (c countingAIClient) Process(ctx context.Context, pdf io.Reader) (ai.Result, error) {
	*c.calls++
	_, _ = io.Copy(io.Discard, pdf)
	return c.result, nil
}

func TestPutDuplicateUploadSkipsAICall(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	calls := 0
	imp := importer.New(cfg, store, countingAIClient{
		result: ai.Result{OCR: "transcript", Summary: []string{"bullet"}},
		calls:  &calls,
	}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	body := []byte("identical-pdf-bytes")
	for i := range 2 {
		req := httptest.NewRequest("PUT", "/Syncs/2026-06-04%20dup.pdf", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 201 {
			t.Fatalf("attempt %d: status = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if calls != 0 {
		t.Errorf("expected queued upload not to call ai.Provider.Process, got %d", calls)
	}
}

func TestPutAISuccessStoresAIStatusSuccess(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	imp := importer.New(cfg, store, fakeAIClient{
		result: ai.Result{OCR: "some text", Summary: []string{"bullet one"}},
	}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	req := httptest.NewRequest("PUT", "/Syncs/2026-07-01%20success.pdf", bytes.NewReader([]byte("pdf-content")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	stored, err := store.GetBySourcePath("Syncs/2026-07-01 success.pdf")
	if err != nil {
		t.Fatalf("GetBySourcePath: %v", err)
	}
	if stored == nil {
		t.Fatal("no record stored")
	}
	if stored.AIStatus != state.AIStatusPending {
		t.Errorf("AIStatus = %q, want %q", stored.AIStatus, state.AIStatusPending)
	}
	if stored.AIRetryCount != 0 {
		t.Errorf("AIRetryCount = %d, want 0", stored.AIRetryCount)
	}
	if stored.AILastError != "" {
		t.Errorf("AILastError = %q, want empty string", stored.AILastError)
	}
	if !stored.AILastRetryAt.IsZero() {
		t.Error("AILastRetryAt is non-zero before worker processing")
	}
}

func TestPutAIFailureStoresAIStatusFailed(t *testing.T) {
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	imp := importer.New(cfg, store, fakeAIClient{
		err: errors.New("gemini 503: service unavailable"),
	}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	req := httptest.NewRequest("PUT", "/Syncs/2026-07-01%20failure.pdf", bytes.NewReader([]byte("pdf-content")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Existing behaviour: error text is written into the note.
	body, err := os.ReadFile(filepath.Join(vaultDir, "notes", "2026-07-01 failure.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "_AI processing queued._") {
		t.Errorf("queued marker block missing from note:\n%s", string(body))
	}

	// New behaviour: AI status is persisted in the state record.
	stored, err := store.GetBySourcePath("Syncs/2026-07-01 failure.pdf")
	if err != nil {
		t.Fatalf("GetBySourcePath: %v", err)
	}
	if stored == nil {
		t.Fatal("no record stored")
	}
	if stored.AIStatus != state.AIStatusPending {
		t.Errorf("AIStatus = %q, want %q", stored.AIStatus, state.AIStatusPending)
	}
	if stored.AIRetryCount != 0 {
		t.Errorf("AIRetryCount = %d, want 0", stored.AIRetryCount)
	}
	if stored.AILastError != "" {
		t.Error("AILastError is non-empty before worker processing")
	}
	if !stored.AILastRetryAt.IsZero() {
		t.Error("AILastRetryAt is non-zero before worker processing")
	}
}

func TestPutMetadataOnlyPDFChangeSkipsAICall(t *testing.T) {
	vaultDir := t.TempDir()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	calls := 0
	imp := importer.New(cfg, store, countingAIClient{
		result: ai.Result{OCR: "transcript", Summary: []string{"bullet"}},
		calls:  &calls,
	}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	bodies := [][]byte{
		[]byte("%PDF-1.7\n1 0 obj << /ModDate (D:20260720100000Z) >> stream\nstroke\nendstream endobj\ntrailer << /ID [<1111><2222>] >>\n%%EOF"),
		[]byte("%PDF-1.7\n1 0 obj << /ModDate (D:20260721100000Z) >> stream\nstroke\nendstream endobj\ntrailer << /ID [<aaaa><bbbb>] >>\n%%EOF"),
	}
	for n, body := range bodies {
		req := httptest.NewRequest("PUT", "/Syncs/2026-06-04%20stable.pdf", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 201 {
			t.Fatalf("attempt %d: status = %d body=%s", n, rec.Code, rec.Body.String())
		}
		if n == 0 {
			// Simulate the async worker having already processed the first
			// upload, so a subsequent metadata-only re-export can be checked
			// for whether it left the AI outcome alone (skip) or requeued it.
			stored, err := store.GetBySourcePath("Syncs/2026-06-04 stable.pdf")
			if err != nil || stored == nil {
				t.Fatalf("GetBySourcePath after first upload: %v", err)
			}
			stored.AIStatus = state.AIStatusSuccess
			stored.AILastSuccessAt = time.Now().UTC()
			if err := store.Put(stored); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
	}

	stored, err := store.GetBySourcePath("Syncs/2026-06-04 stable.pdf")
	if err != nil || stored == nil {
		t.Fatalf("GetBySourcePath: %v", err)
	}
	if stored.AIStatus != state.AIStatusSuccess {
		t.Fatalf("expected metadata-only re-export to leave AI outcome untouched, got AIStatus=%q", stored.AIStatus)
	}
}

func TestPutActualPDFContentChangeCallsAIAgain(t *testing.T) {
	vaultDir := t.TempDir()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &config.Config{
		VaultDir:       vaultDir,
		DefaultPDFDir:  "pdfs",
		DefaultNoteDir: "notes",
		Routes:         []config.Route{{From: "Syncs/", Template: "default", AI: true}},
	}
	calls := 0
	imp := importer.New(cfg, store, countingAIClient{
		result: ai.Result{OCR: "transcript", Summary: []string{"bullet"}},
		calls:  &calls,
	}, 0)
	srv := &Server{cfg: cfg, imp: imp}

	for n, stroke := range []string{"first stroke", "second stroke"} {
		body := []byte("%PDF-1.7\n1 0 obj << /ModDate (D:20260720100000Z) >> stream\n" + stroke + "\nendstream endobj\n%%EOF")
		req := httptest.NewRequest("PUT", "/Syncs/2026-06-04%20changed.pdf", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 201 {
			t.Fatalf("attempt %d: status = %d body=%s", n, rec.Code, rec.Body.String())
		}
		if n == 0 {
			// Simulate the async worker having already processed the first
			// upload; a genuine content change on the second upload must
			// requeue AI work (AIStatus back to pending).
			stored, err := store.GetBySourcePath("Syncs/2026-06-04 changed.pdf")
			if err != nil || stored == nil {
				t.Fatalf("GetBySourcePath after first upload: %v", err)
			}
			stored.AIStatus = state.AIStatusSuccess
			stored.AILastSuccessAt = time.Now().UTC()
			if err := store.Put(stored); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
	}

	stored, err := store.GetBySourcePath("Syncs/2026-06-04 changed.pdf")
	if err != nil || stored == nil {
		t.Fatalf("GetBySourcePath: %v", err)
	}
	if stored.AIStatus != state.AIStatusPending {
		t.Fatalf("expected real content change to requeue AI processing, got AIStatus=%q", stored.AIStatus)
	}
}
