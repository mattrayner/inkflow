package webdavserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// handleMutation dispatches the WebDAV methods which change vault contents.
// Every path reaches the same resolver used by read-only handlers before any
// filesystem operation is attempted.
func (s *Server) handleMutation(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		s.handleDelete(w, r)
	case "COPY":
		s.handleCopy(w, r)
	case "MOVE":
		s.handleMove(w, r)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	clean, target, info, err := s.resolveVaultPath(r.URL.EscapedPath())
	if err != nil {
		writeSourceResolveError(w, err)
		return
	}
	// The vault itself is the WebDAV root, not a resource that a client may
	// remove. This also prevents a malformed DELETE / from emptying the vault.
	if clean == "" {
		http.Error(w, "cannot delete vault root", http.StatusForbidden)
		return
	}
	if !s.requireLocks(w, r, clean) {
		return
	}

	if failures := deleteResource(clean, target, info); len(failures) != 0 {
		writeMutationFailures(w, failures)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request) {
	sourceClean, sourceTarget, sourceInfo, err := s.resolveVaultPath(r.URL.EscapedPath())
	if err != nil {
		writeSourceResolveError(w, err)
		return
	}
	destinationClean, destinationTarget, destinationInfo, err := s.resolveDestination(r)
	if err != nil {
		writeDestinationError(w, err)
		return
	}
	if !s.requireLocks(w, r, destinationClean) {
		return
	}
	if sourceClean == "" || destinationClean == "" || pathsOverlap(sourceClean, destinationClean) {
		http.Error(w, "destination conflicts with source", http.StatusForbidden)
		return
	}
	depth, err := copyDepth(r.Header.Get("Depth"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	overwrite, err := parseOverwrite(r.Header.Get("Overwrite"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if destinationInfo != nil && !overwrite {
		http.Error(w, "destination exists", http.StatusPreconditionFailed)
		return
	}
	if err := validateDestinationParent(destinationTarget); err != nil {
		writeDestinationParentError(w, err)
		return
	}

	existed := destinationInfo != nil
	if existed {
		if failures := deleteResource(destinationClean, destinationTarget, destinationInfo); len(failures) != 0 {
			writeMutationFailures(w, failures)
			return
		}
	}
	if failures := copyResource(sourceClean, sourceTarget, sourceInfo, destinationClean, destinationTarget, depth); len(failures) != 0 {
		writeMutationFailures(w, failures)
		return
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	sourceClean, sourceTarget, _, err := s.resolveVaultPath(r.URL.EscapedPath())
	if err != nil {
		writeSourceResolveError(w, err)
		return
	}
	destinationClean, destinationTarget, destinationInfo, err := s.resolveDestination(r)
	if err != nil {
		writeDestinationError(w, err)
		return
	}
	if !s.requireLocks(w, r, sourceClean, destinationClean) {
		return
	}
	if sourceClean == "" || destinationClean == "" || pathsOverlap(sourceClean, destinationClean) {
		http.Error(w, "destination conflicts with source", http.StatusForbidden)
		return
	}
	overwrite, err := parseOverwrite(r.Header.Get("Overwrite"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if destinationInfo != nil && !overwrite {
		http.Error(w, "destination exists", http.StatusPreconditionFailed)
		return
	}
	if err := validateDestinationParent(destinationTarget); err != nil {
		writeDestinationParentError(w, err)
		return
	}

	existed := destinationInfo != nil
	if !existed {
		if err := os.Rename(sourceTarget, destinationTarget); err != nil {
			http.Error(w, "unable to move resource", statusForError(err))
			return
		}
		w.WriteHeader(http.StatusCreated)
		return
	}

	// Stage the old destination beside its replacement. This avoids a window in
	// which an overwrite has deleted the destination but has not yet moved the
	// source, and lets us roll back if the second rename fails.
	stagedTarget, err := vacantSibling(destinationTarget)
	if err != nil {
		http.Error(w, "unable to stage destination", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(destinationTarget, stagedTarget); err != nil {
		http.Error(w, "unable to stage destination", statusForError(err))
		return
	}
	if err := os.Rename(sourceTarget, destinationTarget); err != nil {
		_ = os.Rename(stagedTarget, destinationTarget)
		http.Error(w, "unable to move resource", statusForError(err))
		return
	}
	stagedInfo, err := os.Lstat(stagedTarget)
	if err != nil {
		http.Error(w, "unable to finalize move", http.StatusInternalServerError)
		return
	}
	if failures := deleteResource(destinationClean, stagedTarget, stagedInfo); len(failures) != 0 {
		writeMutationFailures(w, failures)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveDestination accepts an absolute path or an absolute HTTP(S) URI for
// this request's Host. Cross-server destinations are intentionally unsupported
// because this server has only one local vault.
func (s *Server) resolveDestination(r *http.Request) (string, string, os.FileInfo, error) {
	raw := r.Header.Get("Destination")
	if raw == "" {
		return "", "", nil, errInvalidDestination
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", "", nil, errInvalidDestination
	}
	if u.IsAbs() || u.Host != "" {
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", "", nil, errInvalidDestination
		}
		if !strings.EqualFold(u.Host, r.Host) {
			return "", "", nil, errCrossServerDestination
		}
	}
	if u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return "", "", nil, errInvalidDestination
	}
	return s.resolveVaultPathForCreate(u.EscapedPath())
}

var (
	errInvalidDestination     = errors.New("invalid Destination header")
	errCrossServerDestination = errors.New("cross-server Destination header")
)

func parseOverwrite(value string) (bool, error) {
	switch value {
	case "", "T":
		return true, nil
	case "F":
		return false, nil
	default:
		return false, errors.New("Overwrite must be T or F")
	}
}

func copyDepth(value string) (string, error) {
	switch value {
	case "", "infinity":
		return "infinity", nil
	case "0":
		return "0", nil
	default:
		return "", errors.New("COPY Depth must be 0 or infinity")
	}
}

func validateDestinationParent(target string) error {
	info, err := os.Lstat(filepath.Dir(target))
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("destination parent is not a collection")
	}
	return nil
}

func copyResource(sourceClean, sourceTarget string, sourceInfo os.FileInfo, destinationClean, destinationTarget, depth string) []mutationFailure {
	if sourceInfo.Mode()&os.ModeSymlink != 0 {
		return []mutationFailure{{Clean: sourceClean, Status: http.StatusForbidden}}
	}
	if !sourceInfo.IsDir() {
		if err := copyFile(sourceTarget, destinationTarget, sourceInfo.Mode()); err != nil {
			return []mutationFailure{{Clean: destinationClean, Status: statusForError(err)}}
		}
		return nil
	}
	if err := os.Mkdir(destinationTarget, sourceInfo.Mode().Perm()); err != nil {
		return []mutationFailure{{Clean: destinationClean, Status: statusForError(err)}}
	}
	if depth == "0" {
		return nil
	}
	entries, err := os.ReadDir(sourceTarget)
	if err != nil {
		return []mutationFailure{{Clean: sourceClean, Status: statusForError(err)}}
	}
	var failures []mutationFailure
	for _, entry := range entries {
		sourceChildClean := path.Join(sourceClean, entry.Name())
		destinationChildClean := path.Join(destinationClean, entry.Name())
		sourceChildTarget := filepath.Join(sourceTarget, entry.Name())
		destinationChildTarget := filepath.Join(destinationTarget, entry.Name())
		info, err := os.Lstat(sourceChildTarget)
		if err != nil {
			failures = append(failures, mutationFailure{Clean: sourceChildClean, Status: statusForError(err)})
			continue
		}
		failures = append(failures, copyResource(sourceChildClean, sourceChildTarget, info, destinationChildClean, destinationChildTarget, depth)...)
	}
	return failures
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// deleteResource deliberately does not follow symlinks, even when deleting a
// collection recursively. A symlink is reported as a per-resource failure
// rather than being allowed to point a destructive operation outside the vault.
func deleteResource(clean, target string, info os.FileInfo) []mutationFailure {
	if info.Mode()&os.ModeSymlink != 0 {
		return []mutationFailure{{Clean: clean, Status: http.StatusForbidden}}
	}
	if !info.IsDir() {
		if err := os.Remove(target); err != nil {
			return []mutationFailure{{Clean: clean, Status: statusForError(err)}}
		}
		return nil
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return []mutationFailure{{Clean: clean, Status: statusForError(err)}}
	}
	var failures []mutationFailure
	for _, entry := range entries {
		childClean := path.Join(clean, entry.Name())
		childTarget := filepath.Join(target, entry.Name())
		childInfo, err := os.Lstat(childTarget)
		if err != nil {
			failures = append(failures, mutationFailure{Clean: childClean, Status: statusForError(err)})
			continue
		}
		failures = append(failures, deleteResource(childClean, childTarget, childInfo)...)
	}
	if err := os.Remove(target); err != nil {
		failures = append(failures, mutationFailure{Clean: clean, Status: statusForError(err)})
	}
	return failures
}

func vacantSibling(target string) (string, error) {
	parent := filepath.Dir(target)
	for range 16 {
		bytes := make([]byte, 8)
		if _, err := rand.Read(bytes); err != nil {
			return "", err
		}
		candidate := filepath.Join(parent, ".inkflow-move-"+hex.EncodeToString(bytes))
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("unable to allocate staging path")
}

func sameOrDescendant(source, destination string) bool {
	return destination == source || strings.HasPrefix(destination, source+"/")
}

// pathsOverlap prevents an overwrite from deleting the source when the
// destination is an ancestor, as well as preventing a collection from being
// copied or moved into itself or one of its descendants.
func pathsOverlap(source, destination string) bool {
	return sameOrDescendant(source, destination) || sameOrDescendant(destination, source)
}

func writeSourceResolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, "invalid path", http.StatusBadRequest)
}

func writeDestinationError(w http.ResponseWriter, err error) {
	if errors.Is(err, errCrossServerDestination) {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "destination parent not found", http.StatusConflict)
		return
	}
	http.Error(w, "invalid Destination header", http.StatusBadRequest)
}

func writeDestinationParentError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "destination parent not found", http.StatusConflict)
		return
	}
	http.Error(w, "invalid destination parent", http.StatusConflict)
}

func statusForError(err error) int {
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusNotFound
	}
	if errors.Is(err, os.ErrPermission) {
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

type mutationFailure struct {
	Clean  string
	Status int
}

type mutationMultistatus struct {
	XMLName   xml.Name                  `xml:"D:multistatus"`
	XMLNSD    string                    `xml:"xmlns:D,attr"`
	Responses []mutationFailureResponse `xml:"D:response"`
}

type mutationFailureResponse struct {
	Href   string `xml:"D:href"`
	Status string `xml:"D:status"`
}

func writeMutationFailures(w http.ResponseWriter, failures []mutationFailure) {
	responses := make([]mutationFailureResponse, 0, len(failures))
	for _, failure := range failures {
		responses = append(responses, mutationFailureResponse{
			Href:   escapeHref("/" + strings.TrimPrefix(failure.Clean, "/")),
			Status: fmt.Sprintf("HTTP/1.1 %d %s", failure.Status, http.StatusText(failure.Status)),
		})
	}
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	_ = xml.NewEncoder(w).Encode(mutationMultistatus{XMLName: xml.Name{Space: "DAV:", Local: "multistatus"}, XMLNSD: "DAV:", Responses: responses})
}
