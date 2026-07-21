package webdavserver

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"inkflow/internal/state"
)

const (
	defaultLockTimeout = time.Hour
	maxLockTimeout     = 24 * time.Hour
)

func (s *Server) handleLock(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.WebDAV.EnableLocking {
		s.methodNotAllowed(w)
		return
	}
	if s.store == nil {
		http.Error(w, "locking unavailable", http.StatusServiceUnavailable)
		return
	}
	clean, _, info, err := s.resolveVaultPathForCreate(r.URL.EscapedPath())
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	timeout, err := parseLockTimeout(r.Header.Get("Timeout"))
	if err != nil {
		http.Error(w, "invalid Timeout", http.StatusBadRequest)
		return
	}
	body, err := readXMLBody(r.Body)
	if err != nil {
		http.Error(w, "invalid LOCK body", http.StatusBadRequest)
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		s.refreshLock(w, r, clean, timeout)
		return
	}
	owner, err := parseLockInfo(body)
	if err != nil {
		http.Error(w, "invalid LOCK body", http.StatusBadRequest)
		return
	}
	depth, err := lockDepth(r.Header.Get("Depth"), info != nil && info.IsDir())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	token, err := newLockToken()
	if err != nil {
		http.Error(w, "unable to create lock", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	lock := state.LockRecord{Token: token, ResourcePath: clean, Depth: depth, Owner: owner, CreatedAt: now, ExpiresAt: now.Add(timeout)}
	created, err := s.store.CreateLock(lock)
	if err != nil {
		s.error("create lock", "path", clean, "err", err)
		http.Error(w, "locking unavailable", http.StatusInternalServerError)
		return
	}
	if !created {
		http.Error(w, "resource is locked", http.StatusLocked)
		return
	}
	s.writeLockResponse(w, http.StatusOK, lock)
}

func (s *Server) refreshLock(w http.ResponseWriter, r *http.Request, clean string, timeout time.Duration) {
	for token := range parseIfTokens(r.Header.Get("If")) {
		lock, err := s.store.RefreshLock(token, clean, time.Now().UTC().Add(timeout))
		if err != nil {
			s.error("refresh lock", "path", clean, "err", err)
			http.Error(w, "locking unavailable", http.StatusInternalServerError)
			return
		}
		if lock != nil {
			s.writeLockResponse(w, http.StatusOK, *lock)
			return
		}
	}
	http.Error(w, "lock token does not match resource", http.StatusPreconditionFailed)
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.WebDAV.EnableLocking {
		s.methodNotAllowed(w)
		return
	}
	if s.store == nil {
		http.Error(w, "locking unavailable", http.StatusServiceUnavailable)
		return
	}
	clean, _, _, err := s.resolveVaultPathForCreate(r.URL.EscapedPath())
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	token := parseLockToken(r.Header.Get("Lock-Token"))
	if token == "" {
		http.Error(w, "missing Lock-Token", http.StatusBadRequest)
		return
	}
	removed, err := s.store.Unlock(token, clean)
	if err != nil {
		s.error("unlock", "path", clean, "err", err)
		http.Error(w, "locking unavailable", http.StatusInternalServerError)
		return
	}
	if !removed {
		http.Error(w, "lock token does not match resource", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireLocks(w http.ResponseWriter, r *http.Request, paths ...string) bool {
	if !s.cfg.WebDAV.EnableLocking {
		return true
	}
	if s.store == nil {
		http.Error(w, "locking unavailable", http.StatusServiceUnavailable)
		return false
	}
	satisfied, err := s.store.LocksSatisfied(paths, parseIfTokens(r.Header.Get("If")))
	if err != nil {
		s.error("check locks", "paths", paths, "err", err)
		http.Error(w, "locking unavailable", http.StatusInternalServerError)
		return false
	}
	if !satisfied {
		http.Error(w, "resource is locked", http.StatusLocked)
		return false
	}
	return true
}

func parseIfTokens(value string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for {
		start := strings.IndexByte(value, '<')
		if start < 0 {
			return tokens
		}
		value = value[start+1:]
		end := strings.IndexByte(value, '>')
		if end < 0 {
			return tokens
		}
		token := value[:end]
		if strings.HasPrefix(token, "opaquelocktoken:") {
			tokens[token] = struct{}{}
		}
		value = value[end+1:]
	}
}

func parseLockToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '<' && value[len(value)-1] == '>' {
		value = value[1 : len(value)-1]
	}
	if strings.HasPrefix(value, "opaquelocktoken:") {
		return value
	}
	return ""
}

func parseLockTimeout(value string) (time.Duration, error) {
	if value == "" {
		return defaultLockTimeout, nil
	}
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if strings.EqualFold(candidate, "Infinite") {
			return maxLockTimeout, nil
		}
		if strings.HasPrefix(candidate, "Second-") {
			seconds, err := strconv.ParseInt(strings.TrimPrefix(candidate, "Second-"), 10, 64)
			if err == nil && seconds > 0 {
				if seconds > int64(maxLockTimeout/time.Second) {
					return maxLockTimeout, nil
				}
				return time.Duration(seconds) * time.Second, nil
			}
		}
	}
	return 0, fmt.Errorf("invalid Timeout")
}

func lockDepth(value string, isCollection bool) (string, error) {
	if value == "" || value == "0" {
		return "0", nil
	}
	if value == "infinity" && isCollection {
		return "infinity", nil
	}
	return "", fmt.Errorf("LOCK Depth must be 0 or infinity on a collection")
}

func newLockToken() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "opaquelocktoken:" + hex.EncodeToString(bytes), nil
}

func parseLockInfo(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	root, err := nextStart(decoder)
	if err != nil || root.Name.Space != davNamespace || root.Name.Local != "lockinfo" {
		return "", fmt.Errorf("expected DAV:lockinfo")
	}
	var exclusive, write bool
	var owner string
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch token := token.(type) {
		case xml.StartElement:
			switch {
			case token.Name.Space == davNamespace && token.Name.Local == "exclusive":
				exclusive = true
				if err := decoder.Skip(); err != nil {
					return "", err
				}
			case token.Name.Space == davNamespace && token.Name.Local == "write":
				write = true
				if err := decoder.Skip(); err != nil {
					return "", err
				}
			case token.Name.Space == davNamespace && token.Name.Local == "owner":
				var value struct {
					Inner string `xml:",innerxml"`
				}
				if err := decoder.DecodeElement(&value, &token); err != nil {
					return "", err
				}
				owner = value.Inner
			}
		case xml.EndElement:
			if token.Name == root.Name {
				if !exclusive || !write {
					return "", fmt.Errorf("only exclusive write locks are supported")
				}
				return owner, nil
			}
		}
	}
}

func (s *Server) writeLockResponse(w http.ResponseWriter, status int, lock state.LockRecord) {
	w.Header().Set("Lock-Token", "<"+lock.Token+">")
	w.Header().Set("Timeout", timeoutHeader(lock.ExpiresAt))
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(lockResponse{Lock: lock})
}

func timeoutHeader(expiresAt time.Time) string {
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		return "Second-1"
	}
	seconds := int64((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf("Second-%d", seconds)
}

type lockResponse struct{ Lock state.LockRecord }

func (r lockResponse) MarshalXML(encoder *xml.Encoder, _ xml.StartElement) error {
	start := xml.StartElement{Name: xml.Name{Local: "D:prop"}, Attr: []xml.Attr{{Name: xml.Name{Local: "xmlns:D"}, Value: davNamespace}}}
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	if err := encoder.Encode(davProperty{name: propertyName{Namespace: davNamespace, Local: "lockdiscovery"}, value: lockDiscoveryValue([]state.LockRecord{r.Lock}), raw: true}); err != nil {
		return err
	}
	return encoder.EncodeToken(start.End())
}

func supportedLockValue() string {
	return "<D:lockentry><D:lockscope><D:exclusive></D:exclusive></D:lockscope><D:locktype><D:write></D:write></D:locktype></D:lockentry>"
}

func lockDiscoveryValue(locks []state.LockRecord) string {
	var value strings.Builder
	for _, lock := range locks {
		value.WriteString("<D:activelock><D:locktype><D:write></D:write></D:locktype><D:lockscope><D:exclusive></D:exclusive></D:lockscope>")
		value.WriteString("<D:depth>")
		value.WriteString(lock.Depth)
		value.WriteString("</D:depth>")
		if lock.Owner != "" {
			value.WriteString("<D:owner>")
			value.WriteString(lock.Owner)
			value.WriteString("</D:owner>")
		}
		value.WriteString("<D:timeout>")
		value.WriteString(timeoutHeader(lock.ExpiresAt))
		value.WriteString("</D:timeout><D:locktoken><D:href>")
		value.WriteString(lock.Token)
		value.WriteString("</D:href></D:locktoken></D:activelock>")
	}
	return value.String()
}
