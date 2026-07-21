package webdavserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
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

	"inkflow/internal/config"
	"inkflow/internal/importer"
	"inkflow/internal/observability"
	"inkflow/internal/state"
)

const shutdownTimeout = 10 * time.Second
const defaultMaxUploadBytes int64 = 100 * 1024 * 1024

type Server struct {
	cfg      *config.Config
	imp      *importer.Importer
	store    *state.Store
	metrics  *observability.Metrics
	logger   *slog.Logger
	resolver vaultResolver
	allow    string
	dav      string
}

func Serve(ctx context.Context, cfg *config.Config, imp *importer.Importer, store *state.Store, metrics *observability.Metrics, logger *slog.Logger) error {
	if cfg.WebDAVUser == "" {
		cfg.WebDAVUser = os.Getenv("WEBDAV_USER")
	}
	if cfg.WebDAVPass == "" {
		cfg.WebDAVPass = os.Getenv("WEBDAV_PASS")
	}
	srv := newServer(cfg, imp, store, metrics, logger)
	if cfg.WebDAVUser == "" && cfg.WebDAVPass == "" && !isLoopbackListenAddr(cfg.ListenAddr) {
		srv.warn("UNSAFE WEBDAV CONFIGURATION: unauthenticated plaintext vault writes are reachable on a non-loopback address; configure credentials and use TLS via a reverse proxy", "listen_addr", cfg.ListenAddr)
	}
	httpSrv := newHTTPServer(cfg, srv)

	go func() {
		<-ctx.Done()
		srv.shutdown(httpSrv)
	}()

	srv.info("webdav server starting", "listen_addr", cfg.ListenAddr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func newServer(cfg *config.Config, imp *importer.Importer, store *state.Store, metrics *observability.Metrics, logger *slog.Logger) *Server {
	allow, dav := capabilityHeaders(cfg)
	return &Server{
		cfg:      cfg,
		imp:      imp,
		store:    store,
		metrics:  metrics,
		logger:   logger,
		resolver: newVaultResolver(cfg.VaultDir),
		allow:    allow,
		dav:      dav,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clean := cleanPath(r.URL.Path)
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		s.handleHealth(w)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/metrics" && s.cfg.Observability.MetricsEnabled && s.cfg.Observability.MetricsAddr == "" {
		if !s.authorize(w, r) {
			return
		}
		s.metrics.Handler().ServeHTTP(w, r)
		return
	}
	if !s.authorize(w, r) {
		return
	}
	defer r.Body.Close()

	s.info("webdav request", "method", r.Method, "path", clean, "depth", r.Header.Get("Depth"))
	switch r.Method {
	case http.MethodOptions:
		s.setCapabilityHeaders(w)
		w.WriteHeader(http.StatusNoContent)
	case "PROPFIND":
		s.handlePropfind(w, r)
	case "MKCOL":
		s.handleMkcol(w, r, clean)
	case http.MethodPut:
		s.handlePut(w, r, clean)
	case http.MethodGet, http.MethodHead:
		if !s.cfg.WebDAV.EnableRetrieval {
			s.methodNotAllowed(w)
			return
		}
		s.handleGetOrHead(w, r)
	default:
		s.methodNotAllowed(w)
	}
}

func capabilityHeaders(cfg *config.Config) (allow, dav string) {
	methods := []string{"OPTIONS", "PROPFIND", "MKCOL", "PUT"}
	if cfg != nil && cfg.WebDAV.EnableRetrieval {
		methods = append(methods, "GET", "HEAD")
	}
	// This increment implements no locking methods, so it must never advertise
	// DAV Class 2 even if its future-facing configuration flag is set.
	return strings.Join(methods, ", "), "1"
}

func (s *Server) setCapabilityHeaders(w http.ResponseWriter) {
	allow, dav := s.allow, s.dav
	if allow == "" { // Supports package-local test servers constructed as literals.
		allow, dav = capabilityHeaders(s.cfg)
	}
	w.Header().Set("Allow", allow)
	w.Header().Set("DAV", dav)
}

func (s *Server) methodNotAllowed(w http.ResponseWriter) {
	s.setCapabilityHeaders(w)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	clean := cleanPath(r.URL.Path)
	if s.cfg.WebDAVUser == "" && s.cfg.WebDAVPass == "" {
		s.debug("auth_check", "authenticated", true, "path", clean)
		return true
	}
	user, pass, ok := r.BasicAuth()
	configuredUser := sha256.Sum256([]byte(s.cfg.WebDAVUser))
	suppliedUser := sha256.Sum256([]byte(user))
	configuredPass := sha256.Sum256([]byte(s.cfg.WebDAVPass))
	suppliedPass := sha256.Sum256([]byte(pass))
	if ok && subtle.ConstantTimeCompare(suppliedUser[:], configuredUser[:]) == 1 && subtle.ConstantTimeCompare(suppliedPass[:], configuredPass[:]) == 1 {
		s.debug("auth_check", "authenticated", true, "path", clean)
		return true
	}
	s.debug("auth_check", "authenticated", false, "path", clean)
	s.debug("auth_rejected", "path", clean)
	w.Header().Set("WWW-Authenticate", `Basic realm="inkflow"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, clean string) {
	route := s.routeLabel(clean)
	if clean == "" {
		s.metrics.Import(route, "rejected")
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	maxUploadBytes := s.cfg.MaxUploadBytes
	if maxUploadBytes == 0 {
		maxUploadBytes = defaultMaxUploadBytes
	}
	if r.ContentLength > maxUploadBytes {
		s.metrics.Import(route, "rejected")
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	started := time.Now().UTC()
	s.debug("import_dispatch", "path", clean)
	rec, err := s.imp.Import(r.Context(), clean, body, started)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			s.metrics.Import(route, "rejected")
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
		s.error("webdav import failed", "path", clean, "err", err)
		s.metrics.Import(route, "failed")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if rec.ImportedAt.Before(started) {
		s.metrics.Import(route, "deduplicated")
		s.metrics.DedupSkip()
	} else {
		s.metrics.Import(route, "succeeded")
	}
	s.info("webdav imported", "path", clean, "note", rec.VaultNotePath, "pdf", rec.VaultPDFPath)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleGetOrHead(w http.ResponseWriter, r *http.Request) {
	clean, target, info, err := s.resolveVaultPath(r.URL.EscapedPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	if info.IsDir() {
		w.Header().Set("Content-Type", "httpd/unix-directory")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}

	etag, err := s.etag(clean, target)
	if err != nil {
		s.error("compute ETag", "path", clean, "err", err)
		http.Error(w, "unable to read resource", http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(target)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if notModified(r, etag, info.ModTime()) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	file, err := os.Open(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "unable to read resource", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func notModified(r *http.Request, etag string, modTime time.Time) bool {
	if values, ok := r.Header["If-None-Match"]; ok {
		for _, value := range values {
			for _, candidate := range strings.Split(value, ",") {
				candidate = strings.TrimSpace(candidate)
				if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
					return true
				}
			}
		}
		return false
	}
	if modifiedSince := r.Header.Get("If-Modified-Since"); modifiedSince != "" {
		if since, err := http.ParseTime(modifiedSince); err == nil {
			return !modTime.UTC().Truncate(time.Second).After(since)
		}
	}
	return false
}

func (s *Server) handleHealth(w http.ResponseWriter) {
	if s.store == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	pending, err := s.store.GetPendingAndFailedAIImports()
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.metrics != nil {
		s.metrics.QueueDepth(len(pending))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) routeLabel(clean string) string {
	best := ""
	for _, route := range s.cfg.Routes {
		prefix := config.NormalizeRoutePrefix(route.From)
		if strings.HasPrefix(clean+"/", strings.TrimPrefix(prefix, "/")) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return "unmatched"
	}
	return best
}

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request) {
	clean, target, info, err := s.resolveVaultPath(r.URL.EscapedPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	depth := r.Header.Get("Depth")
	if depth != "" && depth != "0" && depth != "1" {
		http.Error(w, "unsupported Depth", http.StatusBadRequest)
		return
	}
	responses := []propResponse{s.responseFor(clean, info)}
	if depth == "1" && info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			http.Error(w, "unable to list resource", http.StatusInternalServerError)
			return
		}
		for _, entry := range entries {
			childClean := path.Join(clean, entry.Name())
			_, _, childInfo, err := s.resolveVaultPath("/" + childClean)
			if err != nil {
				// Do not follow a symlink (including one that escapes the vault).
				continue
			}
			responses = append(responses, s.responseFor(childClean, childInfo))
		}
	}
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	_ = xml.NewEncoder(w).Encode(multistatus{XMLName: xml.Name{Space: "DAV:", Local: "multistatus"}, XMLNSD: "DAV:", Responses: responses})
}

func (s *Server) handleMkcol(w http.ResponseWriter, r *http.Request, clean string) {
	_, target, info, err := s.resolveVaultPathForCreate(r.URL.EscapedPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.debug("mkcol_conflict", "path", clean, "reason", "parent_not_found")
			http.Error(w, "parent collection not found", http.StatusConflict)
			return
		}
		s.debug("mkcol_rejected", "path", clean, "err", err)
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if info != nil {
		if info.IsDir() {
			s.debug("mkcol_already_exists", "path", clean)
			http.Error(w, "collection already exists", http.StatusMethodNotAllowed)
			return
		}
		s.debug("mkcol_conflict", "path", clean, "reason", "target_is_file")
		http.Error(w, "target is not a collection", http.StatusConflict)
		return
	}

	parentInfo, err := os.Lstat(filepath.Dir(target))
	if err != nil || !parentInfo.IsDir() {
		s.debug("mkcol_conflict", "path", clean, "reason", "parent_not_found")
		http.Error(w, "parent collection not found", http.StatusConflict)
		return
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 {
		s.debug("mkcol_rejected", "path", clean, "err", "symlink parent")
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		if os.IsExist(err) {
			if info, statErr := os.Lstat(target); statErr == nil && !info.IsDir() {
				s.debug("mkcol_conflict", "path", clean, "reason", "target_is_file")
				http.Error(w, "target is not a collection", http.StatusConflict)
				return
			}
			s.debug("mkcol_already_exists", "path", clean)
			http.Error(w, "collection already exists", http.StatusMethodNotAllowed)
			return
		}
		s.error("webdav mkcol failed", "path", clean, "err", err)
		http.Error(w, "unable to create collection", http.StatusInternalServerError)
		return
	}
	s.debug("mkcol_created", "path", clean)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) responseFor(clean string, info os.FileInfo) propResponse {
	href := "/" + strings.TrimPrefix(clean, "/")
	href = escapeHref(href)
	prop := prop{
		ResourceType: resourceType{},
		LastModified: info.ModTime().UTC().Format(http.TimeFormat),
	}
	if clean == "" {
		prop.Displayname = info.Name()
	} else {
		prop.Displayname = path.Base(clean)
	}
	if info.IsDir() {
		href = strings.TrimSuffix(href, "/") + "/"
		prop.ResourceType.Collection = &struct{}{}
		prop.ContentType = "httpd/unix-directory"
	} else {
		size := info.Size()
		prop.ContentLength = &size
		prop.ContentType = "application/octet-stream"
	}
	return propResponse{Href: href, Propstat: propstat{Prop: prop, Status: "HTTP/1.1 200 OK"}}
}

func (s *Server) resolveVaultPath(rawPath string) (string, string, os.FileInfo, error) {
	return s.pathResolver().resolve(rawPath)
}

func (s *Server) resolveVaultPathForCreate(rawPath string) (string, string, os.FileInfo, error) {
	return s.pathResolver().resolveForCreate(rawPath)
}

func (s *Server) pathResolver() vaultResolver {
	if s.resolver.vaultDir != "" {
		return s.resolver
	}
	return newVaultResolver(s.cfg.VaultDir)
}

func escapeHref(href string) string {
	parts := strings.Split(strings.TrimPrefix(href, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/" + strings.Join(parts, "/")
}

func (s *Server) info(msg string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Info(msg, args...)
	}
}

func (s *Server) debug(msg string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Debug(msg, args...)
	}
}

func (s *Server) error(msg string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Error(msg, args...)
	}
}

func (s *Server) warn(msg string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}

func (s *Server) shutdown(httpSrv *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		s.error("webdav server shutdown failed", "err", err)
	}
}

func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newHTTPServer(cfg *config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeoutDuration,
		ReadTimeout:       cfg.ReadTimeoutDuration,
		WriteTimeout:      cfg.WriteTimeoutDuration,
		IdleTimeout:       cfg.IdleTimeoutDuration,
	}
}

type multistatus struct {
	XMLName   xml.Name       `xml:"D:multistatus"`
	XMLNSD    string         `xml:"xmlns:D,attr"`
	Responses []propResponse `xml:"D:response"`
}

type propResponse struct {
	Href     string   `xml:"D:href"`
	Propstat propstat `xml:"D:propstat"`
}

type propstat struct {
	Prop   prop   `xml:"D:prop"`
	Status string `xml:"D:status"`
}

type prop struct {
	ResourceType  resourceType `xml:"D:resourcetype"`
	Displayname   string       `xml:"D:displayname,omitempty"`
	LastModified  string       `xml:"D:getlastmodified,omitempty"`
	ContentLength *int64       `xml:"D:getcontentlength,omitempty"`
	ContentType   string       `xml:"D:getcontenttype,omitempty"`
}

type resourceType struct {
	Collection *struct{} `xml:"D:collection,omitempty"`
}
