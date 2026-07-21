package webdavserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"inkflow/internal/config"
	"inkflow/internal/importer"
	"inkflow/internal/state"
)

const exclusiveWriteLockBody = `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>Inkflow test</D:owner></D:lockinfo>`

func newLockingTestServer(t *testing.T) (*Server, string, *state.Store) {
	t.Helper()
	vault := t.TempDir()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &config.Config{VaultDir: vault, DefaultPDFDir: "pdfs", DefaultNoteDir: "notes", WebDAV: config.WebDAVConfig{EnableMutation: true, EnableLocking: true}, Routes: []config.Route{{From: "Syncs/"}}}
	return newServer(cfg, importer.New(cfg, store, nil, 0), store, nil, nil), vault, store
}

func lockResource(t *testing.T, srv *Server, resource string, depth string) string {
	t.Helper()
	req := httptest.NewRequest("LOCK", resource, strings.NewReader(exclusiveWriteLockBody))
	req.Header.Set("Timeout", "Second-60")
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK %s = %d: %s", resource, rec.Code, rec.Body.String())
	}
	token := rec.Header().Get("Lock-Token")
	if !strings.HasPrefix(token, "<opaquelocktoken:") || !strings.Contains(rec.Header().Get("Timeout"), "Second-") || !strings.Contains(rec.Body.String(), "<D:lockdiscovery>") || !strings.Contains(rec.Body.String(), "Inkflow test") {
		t.Fatalf("invalid LOCK response: headers=%v body=%s", rec.Header(), rec.Body.String())
	}
	return strings.TrimSuffix(strings.TrimPrefix(token, "<"), ">")
}

func TestLockingConfigurationAndLifecycle(t *testing.T) {
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	disabled := &Server{cfg: &config.Config{VaultDir: vault}}
	rec := httptest.NewRecorder()
	disabled.ServeHTTP(rec, httptest.NewRequest("LOCK", "/note.txt", strings.NewReader(exclusiveWriteLockBody)))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("DAV") != "1" {
		t.Fatalf("disabled LOCK = %d DAV=%q", rec.Code, rec.Header().Get("DAV"))
	}
	disabledUnlock := httptest.NewRecorder()
	disabled.ServeHTTP(disabledUnlock, httptest.NewRequest("UNLOCK", "/note.txt", nil))
	if disabledUnlock.Code != http.StatusMethodNotAllowed {
		t.Fatalf("disabled UNLOCK = %d", disabledUnlock.Code)
	}

	srv, vault, _ := newLockingTestServer(t)
	if err := os.WriteFile(filepath.Join(vault, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := httptest.NewRecorder()
	srv.ServeHTTP(options, httptest.NewRequest(http.MethodOptions, "/", nil))
	if options.Header().Get("DAV") != "1, 2" || !strings.Contains(options.Header().Get("Allow"), "LOCK") || !strings.Contains(options.Header().Get("Allow"), "UNLOCK") {
		t.Fatalf("locking capabilities: DAV=%q Allow=%q", options.Header().Get("DAV"), options.Header().Get("Allow"))
	}
	token := lockResource(t, srv, "/note.txt", "")

	duplicate := httptest.NewRecorder()
	srv.ServeHTTP(duplicate, httptest.NewRequest("LOCK", "/note.txt", strings.NewReader(exclusiveWriteLockBody)))
	if duplicate.Code != http.StatusLocked {
		t.Fatalf("duplicate LOCK = %d: %s", duplicate.Code, duplicate.Body.String())
	}
	badUnlock := httptest.NewRecorder()
	badUnlockRequest := httptest.NewRequest("UNLOCK", "/note.txt", nil)
	badUnlockRequest.Header.Set("Lock-Token", "<opaquelocktoken:wrong>")
	srv.ServeHTTP(badUnlock, badUnlockRequest)
	if badUnlock.Code != http.StatusConflict {
		t.Fatalf("invalid UNLOCK = %d", badUnlock.Code)
	}
	unlock := httptest.NewRecorder()
	unlockRequest := httptest.NewRequest("UNLOCK", "/note.txt", nil)
	unlockRequest.Header.Set("Lock-Token", "<"+token+">")
	srv.ServeHTTP(unlock, unlockRequest)
	if unlock.Code != http.StatusNoContent {
		t.Fatalf("UNLOCK = %d: %s", unlock.Code, unlock.Body.String())
	}
}

func TestLockEnforcementNullResourcesAndDepth(t *testing.T) {
	srv, vault, _ := newLockingTestServer(t)
	if err := os.Mkdir(filepath.Join(vault, "collection"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "collection", "child.txt"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(vault, "Syncs"), 0o755); err != nil {
		t.Fatal(err)
	}
	depthToken := lockResource(t, srv, "/collection", "infinity")
	patchBody := `<D:propertyupdate xmlns:D="DAV:" xmlns:X="urn:test"><D:set><D:prop><X:tag>value</X:tag></D:prop></D:set></D:propertyupdate>`
	lockedPatch := httptest.NewRecorder()
	srv.ServeHTTP(lockedPatch, httptest.NewRequest("PROPPATCH", "/collection/child.txt", strings.NewReader(patchBody)))
	if lockedPatch.Code != http.StatusLocked {
		t.Fatalf("descendant PROPPATCH without token = %d", lockedPatch.Code)
	}
	allowedPatch := httptest.NewRecorder()
	allowedPatchRequest := httptest.NewRequest("PROPPATCH", "/collection/child.txt", strings.NewReader(patchBody))
	allowedPatchRequest.Header.Set("If", "(<"+depthToken+">)")
	srv.ServeHTTP(allowedPatch, allowedPatchRequest)
	if allowedPatch.Code != http.StatusMultiStatus {
		t.Fatalf("descendant PROPPATCH with token = %d: %s", allowedPatch.Code, allowedPatch.Body.String())
	}

	nullToken := lockResource(t, srv, "/Syncs/2026-07-21%20null.pdf", "")
	blockedPut := httptest.NewRecorder()
	srv.ServeHTTP(blockedPut, httptest.NewRequest(http.MethodPut, "/Syncs/2026-07-21%20null.pdf", bytes.NewBufferString("pdf")))
	if blockedPut.Code != http.StatusLocked {
		t.Fatalf("null PUT without token = %d", blockedPut.Code)
	}
	allowedPut := httptest.NewRecorder()
	allowedPutRequest := httptest.NewRequest(http.MethodPut, "/Syncs/2026-07-21%20null.pdf", bytes.NewBufferString("pdf"))
	allowedPutRequest.Header.Set("If", "(<"+nullToken+">)")
	srv.ServeHTTP(allowedPut, allowedPutRequest)
	if allowedPut.Code != http.StatusCreated {
		t.Fatalf("null PUT with token = %d: %s", allowedPut.Code, allowedPut.Body.String())
	}
	if err := os.Mkdir(filepath.Join(vault, "shallow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "shallow", "child.txt"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockResource(t, srv, "/shallow", "0")
	shallowPatch := httptest.NewRecorder()
	srv.ServeHTTP(shallowPatch, httptest.NewRequest("PROPPATCH", "/shallow/child.txt", strings.NewReader(patchBody)))
	if shallowPatch.Code != http.StatusMultiStatus {
		t.Fatalf("Depth 0 lock covered child = %d: %s", shallowPatch.Code, shallowPatch.Body.String())
	}
}

func TestLockEnforcedForDeleteCopyMoveAndRefresh(t *testing.T) {
	srv, vault, _ := newLockingTestServer(t)
	for _, name := range []string{"source.txt", "move.txt"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sourceToken := lockResource(t, srv, "/source.txt", "")
	deleteBlocked := httptest.NewRecorder()
	srv.ServeHTTP(deleteBlocked, httptest.NewRequest(http.MethodDelete, "/source.txt", nil))
	if deleteBlocked.Code != http.StatusLocked {
		t.Fatalf("DELETE without token = %d", deleteBlocked.Code)
	}

	copyBlocked := httptest.NewRecorder()
	copyBlockedRequest := httptest.NewRequest("COPY", "/move.txt", nil)
	copyBlockedRequest.Header.Set("Destination", "/source.txt")
	srv.ServeHTTP(copyBlocked, copyBlockedRequest)
	if copyBlocked.Code != http.StatusLocked {
		t.Fatalf("COPY destination without token = %d", copyBlocked.Code)
	}
	copyAllowed := httptest.NewRecorder()
	copyAllowedRequest := httptest.NewRequest("COPY", "/move.txt", nil)
	copyAllowedRequest.Header.Set("Destination", "/source.txt")
	copyAllowedRequest.Header.Set("If", "(<"+sourceToken+">)")
	srv.ServeHTTP(copyAllowed, copyAllowedRequest)
	if copyAllowed.Code != http.StatusNoContent {
		t.Fatalf("COPY destination with token = %d: %s", copyAllowed.Code, copyAllowed.Body.String())
	}

	moveToken := lockResource(t, srv, "/move.txt", "")
	moveBlocked := httptest.NewRecorder()
	moveBlockedRequest := httptest.NewRequest("MOVE", "/move.txt", nil)
	moveBlockedRequest.Header.Set("Destination", "/moved.txt")
	srv.ServeHTTP(moveBlocked, moveBlockedRequest)
	if moveBlocked.Code != http.StatusLocked {
		t.Fatalf("MOVE without token = %d", moveBlocked.Code)
	}
	moveAllowed := httptest.NewRecorder()
	moveAllowedRequest := httptest.NewRequest("MOVE", "/move.txt", nil)
	moveAllowedRequest.Header.Set("Destination", "/moved.txt")
	moveAllowedRequest.Header.Set("If", "(<"+moveToken+">)")
	srv.ServeHTTP(moveAllowed, moveAllowedRequest)
	if moveAllowed.Code != http.StatusCreated {
		t.Fatalf("MOVE with token = %d: %s", moveAllowed.Code, moveAllowed.Body.String())
	}

	refresh := httptest.NewRecorder()
	refreshRequest := httptest.NewRequest("LOCK", "/source.txt", nil)
	refreshRequest.Header.Set("If", "(<"+sourceToken+">)")
	refreshRequest.Header.Set("Timeout", "Second-120")
	srv.ServeHTTP(refresh, refreshRequest)
	if refresh.Code != http.StatusOK || refresh.Header().Get("Lock-Token") != "<"+sourceToken+">" {
		t.Fatalf("LOCK refresh = %d headers=%v body=%s", refresh.Code, refresh.Header(), refresh.Body.String())
	}
}

func TestLockPropertiesAppearInPropfind(t *testing.T) {
	srv, vault, _ := newLockingTestServer(t)
	if err := os.WriteFile(filepath.Join(vault, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	token := lockResource(t, srv, "/note.txt", "")
	req := httptest.NewRequest("PROPFIND", "/note.txt", strings.NewReader(`<D:propfind xmlns:D="DAV:"><D:prop><D:supportedlock/><D:lockdiscovery/></D:prop></D:propfind>`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "<D:supportedlock>") || !strings.Contains(rec.Body.String(), "<D:lockdiscovery>") || !strings.Contains(rec.Body.String(), token) {
		t.Fatalf("lock PROPFIND = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLockTimeoutParsing(t *testing.T) {
	for input, wantOK := range map[string]bool{"": true, "Second-5": true, "Second-1, Infinite": true, "Infinite": true, "Second-0": false, "minutes-1": false} {
		_, err := parseLockTimeout(input)
		if (err == nil) != wantOK {
			t.Errorf("parseLockTimeout(%q) err=%v", input, err)
		}
	}
	if got := timeoutHeader(time.Now().Add(time.Second)); !strings.HasPrefix(got, "Second-") {
		t.Errorf("Timeout header = %q", got)
	}
}
