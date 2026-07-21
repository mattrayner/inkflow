package webdavserver

import (
	"fmt"
	"os"

	"inkflow/internal/util"
)

// etag returns a strong entity tag. Imported PDFs reuse the digest recorded by
// the importer; all other resources are hashed from their current bytes.
func (s *Server) etag(clean, target string) (string, error) {
	if s.store != nil {
		record, err := s.store.GetByVaultPDFPath(clean)
		if err != nil {
			return "", err
		}
		if record != nil && record.SHA256 != "" {
			return fmt.Sprintf("%q", record.SHA256), nil
		}
	}
	file, err := os.Open(target)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash, err := util.SHA256Hex(file)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%q", hash), nil
}
