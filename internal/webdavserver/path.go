package webdavserver

import (
	"errors"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// vaultResolver decodes and validates request targets before they reach the
// filesystem. It rejects traversal and every symlink in the target path so
// DAV handlers cannot expose data outside the configured vault.
type vaultResolver struct {
	vaultDir string
}

func newVaultResolver(vaultDir string) vaultResolver {
	return vaultResolver{vaultDir: vaultDir}
}

func (v vaultResolver) resolve(rawPath string) (string, string, os.FileInfo, error) {
	return v.resolveTarget(rawPath, false)
}

func (v vaultResolver) resolveForCreate(rawPath string) (string, string, os.FileInfo, error) {
	return v.resolveTarget(rawPath, true)
}

func (v vaultResolver) resolveTarget(rawPath string, allowFinalMissing bool) (string, string, os.FileInfo, error) {
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", "", nil, err
	}
	if strings.Contains(decoded, "\\") {
		return "", "", nil, errors.New("backslash path separator")
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == ".." {
			return "", "", nil, errors.New("path traversal")
		}
	}
	clean := cleanPath(decoded)
	root, err := filepath.Abs(v.vaultDir)
	if err != nil {
		return "", "", nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", nil, err
	}
	target := root
	components := strings.Split(clean, "/")
	for index, component := range components {
		if component == "" {
			continue
		}
		target = filepath.Join(target, component)
		info, err := os.Lstat(target)
		if err != nil {
			if allowFinalMissing && os.IsNotExist(err) && index == len(components)-1 {
				return clean, target, nil, nil
			}
			return "", "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", nil, errors.New("symlink target")
		}
	}
	if rel, err := filepath.Rel(root, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", nil, errors.New("path outside vault")
	}
	info, err := os.Lstat(target)
	if err != nil {
		return "", "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", "", nil, errors.New("symlink target")
	}
	return clean, target, info, nil
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
