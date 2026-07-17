package reporting

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cablecheck/internal/model"
)

// RawStore hands out sequenced files under a report's raw/ directory and
// indexes them for the report's raw-artifact section.
//
// File names follow NN-side-tool-purpose.ext with a two-digit creation
// sequence, e.g. raw/03-pc1-iperf3-tcp-pc1-to-pc2.json. The store is safe for
// concurrent Create calls.
type RawStore struct {
	dir string // <reportDir>/raw
	mu  sync.Mutex
	seq int
	// files records the created entries in creation order (relative name +
	// description).
	files []rawEntry
}

// rawEntry is one created raw file.
type rawEntry struct {
	relName     string // relative to the report directory, e.g. "raw/01-..."
	description string
}

// NewRawStore returns a store writing into <reportDir>/raw, creating the
// directory (0700) when it does not exist yet.
func NewRawStore(reportDir string) (*RawStore, error) {
	dir := filepath.Join(reportDir, "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create raw directory: %w", err)
	}
	return &RawStore{dir: dir}, nil
}

// Create opens the next sequenced raw file NN-side-tool-purpose.ext (mode
// 0600) and returns it together with its name relative to the report
// directory. The caller owns the file and must close it before Index.
func (s *RawStore) Create(side, tool, purpose, ext string) (*os.File, string, error) {
	for _, part := range []string{side, tool, purpose, ext} {
		if part == "" || strings.ContainsAny(part, "/\\") || strings.Contains(part, "..") {
			return nil, "", fmt.Errorf("raw file name component %q: empty or contains path characters", part)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	base := fmt.Sprintf("%02d-%s-%s-%s.%s", s.seq, side, tool, purpose, ext)
	f, err := os.OpenFile(filepath.Join(s.dir, base), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		s.seq--
		return nil, "", fmt.Errorf("create raw file: %w", err)
	}
	rel := filepath.Join("raw", base)
	s.files = append(s.files, rawEntry{
		relName:     rel,
		description: fmt.Sprintf("%s %s %s", side, tool, purpose),
	})
	return f, rel, nil
}

// Index returns the raw-file references (name, SHA-256, size) of every file
// created through the store, in creation order. Checksums are computed at
// call time, so all files must be closed (fully flushed) first. Files that
// disappeared or cannot be read are skipped.
func (s *RawStore) Index() []model.RawFileRef {
	s.mu.Lock()
	entries := make([]rawEntry, len(s.files))
	copy(entries, s.files)
	s.mu.Unlock()

	refs := make([]model.RawFileRef, 0, len(entries))
	for _, e := range entries {
		f, err := os.Open(filepath.Join(s.dir, filepath.Base(e.relName)))
		if err != nil {
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			continue
		}
		refs = append(refs, model.RawFileRef{
			Name:        e.relName,
			SHA256:      hex.EncodeToString(h.Sum(nil)),
			Bytes:       n,
			Description: e.description,
		})
	}
	return refs
}
