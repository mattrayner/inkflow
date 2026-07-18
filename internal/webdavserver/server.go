package webdavserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
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
	cfg     *config.Config
	imp     *importer.Importer
	store   *state.Store
	metrics *observability.Metrics
	logger  *slog.Logger
}

func Serve(ctx context.Context, cfg *config.Config, imp *importer.Importer, store *state.Store, metrics *observability.Metrics, logger *slog.Logger) error {
	if cfg.WebDAVUser == "" {
		cfg.WebDAVUser = os.Getenv("WEBDAV_USER")
	}
	if cfg.WebDAVPass == "" {
		cfg.WebDAVPass = os.Getenv("WEBDAV_PASS")
	}
	srv := &Server{cfg: cfg, imp: imp, store: store, metrics: metrics, logger: logger}
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

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	clean := cleanPath(r.URL.Path)
	s.info("webdav request", "method", r.Method, "path", clean, "depth", r.Header.Get("Depth"))
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Allow", "OPTIONS, PROPFIND, PUT")
		w.Header().Set("DAV", "1,2")
		w.WriteHeader(http.StatusNoContent)
	case "PROPFIND":
		s.handlePropfind(w, r, clean)
	case http.MethodPut:
		s.handlePut(w, r, clean)
	default:
		w.Header().Set("Allow", "OPTIONS, PROPFIND, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.WebDAVUser == "" && s.cfg.WebDAVPass == "" {
		return true
	}
	user, pass, ok := r.BasicAuth()
	configuredUser := sha256.Sum256([]byte(s.cfg.WebDAVUser))
	suppliedUser := sha256.Sum256([]byte(user))
	configuredPass := sha256.Sum256([]byte(s.cfg.WebDAVPass))
	suppliedPass := sha256.Sum256([]byte(pass))
	if ok && subtle.ConstantTimeCompare(suppliedUser[:], configuredUser[:]) == 1 && subtle.ConstantTimeCompare(suppliedPass[:], configuredPass[:]) == 1 {
		return true
	}
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

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request, clean string) {
	responses := []propResponse{s.responseFor(clean)}
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	_ = xml.NewEncoder(w).Encode(multistatus{XMLName: xml.Name{Space: "DAV:", Local: "multistatus"}, XMLNSD: "DAV:", Responses: responses})
}

func (s *Server) responseFor(clean string) propResponse {
	if clean == "" {
		return propResponse{Href: "/", Propstat: propstat{Prop: prop{Displayname: "inkflow", ResourceType: resourceType{Collection: &struct{}{}}, ContentType: "httpd/unix-directory"}, Status: "HTTP/1.1 200 OK"}}
	}
	href := "/" + strings.TrimPrefix(clean, "/")
	href = escapeHref(href)
	prop := prop{
		Displayname:  path.Base(strings.TrimSuffix(clean, "/")),
		ResourceType: resourceType{},
		ContentType:  "application/pdf",
	}
	if strings.HasSuffix(clean, "/") {
		href = strings.TrimSuffix(href, "/") + "/"
		prop.ResourceType.Collection = &struct{}{}
		prop.ContentType = "httpd/unix-directory"
	}
	return propResponse{Href: href, Propstat: propstat{Prop: prop, Status: "HTTP/1.1 200 OK"}}
}

func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	p = path.Clean("/" + p)
	p = strings.TrimPrefix(p, "/")
	if p == "." {
		return ""
	}
	return p
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
	ContentLength int64        `xml:"D:getcontentlength,omitempty"`
	ContentType   string       `xml:"D:getcontenttype,omitempty"`
}

type resourceType struct {
	Collection *struct{} `xml:"D:collection,omitempty"`
}
