package reporting

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Report-transfer sizing constants. They mirror the protocol-layer values but
// are redeclared here so the reporting package stays free of any dependency on
// internal/protocol (the dependency-purity test forbids it): the transfer
// logic — manifest build, chunking, digest verification, .part rename — is a
// pure function of files on disk plus a small channel seam, and belongs with
// the rest of the report machinery.
const (
	// ChunkSize is the raw byte size of one report chunk (256 KiB). base64
	// framing at the protocol layer keeps it comfortably under the 1 MiB
	// frame cap.
	ChunkSize = 256 << 10
	// MaxTransferFileSize caps a single transferred report file (8 MiB).
	MaxTransferFileSize = 8 << 20
	// MaxTransferTotal caps the sum of all transferred files (16 MiB).
	MaxTransferTotal = 16 << 20
)

// allowedReportFiles is the exact allowlist of transferable report file names.
// Anything else — including path-traversal names — is rejected by both the
// sender (nothing else is offered) and the receiver (SafeReportName).
var allowedReportFiles = []string{"report.json", "report.md", "summary.txt"}

// TransferFile describes one file in a transfer Manifest: its bare name, byte
// size and hex-encoded whole-file SHA-256 digest.
type TransferFile struct {
	// Name is the bare file name (allowlisted, no path separators).
	Name string
	// Size is the file size in bytes.
	Size int64
	// SHA256 is the hex-encoded digest of the whole file.
	SHA256 string
}

// Manifest opens a report transfer: the files about to be streamed in order,
// plus their total size.
type Manifest struct {
	// Files lists the files to stream, in transfer order.
	Files []TransferFile
	// TotalSize is the sum of all file sizes in bytes.
	TotalSize int64
}

// ChunkFrame carries one slice of a report file from sender to receiver.
type ChunkFrame struct {
	// Name is the manifest file name this chunk belongs to.
	Name string
	// Seq is the 0-based, contiguous chunk index within the file.
	Seq int
	// Offset must equal the receiver's bytes-received count for the file.
	Offset int64
	// Data is the raw chunk (at most ChunkSize bytes).
	Data []byte
	// Last marks the final chunk of the file.
	Last bool
}

// AckFrame acknowledges one transferred file — or declines the whole manifest
// when Declined is set (receiver to sender).
type AckFrame struct {
	// Name is the acknowledged file, or "" for a manifest-level decline.
	Name string
	// OK reports whether the file arrived intact (digest verified).
	OK bool
	// Declined marks a manifest-level refusal (caps, disk, allowlist).
	Declined bool
	// Error describes why OK is false or the manifest was declined.
	Error string
}

// SafeReportName reports whether name is one of the exact allowlisted report
// file names and free of any path-traversal construct. A malicious or buggy
// sender must never be able to make the receiver write outside its report
// directory, so the check is deny-by-default: exact allowlist AND no path
// separator or "..".
func SafeReportName(name string) bool {
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	for _, allowed := range allowedReportFiles {
		if name == allowed {
			return true
		}
	}
	return false
}

// BuildManifest reads the allowlisted report files present in dir (in the
// canonical order report.json, report.md, summary.txt), computing each file's
// size and SHA-256 digest. A file over the per-file cap, or a set over the
// total cap, is an error — the transfer never starts. Missing files are simply
// omitted (a run that produced only some outputs still transfers those).
func BuildManifest(dir string) (Manifest, error) {
	var m Manifest
	for _, name := range allowedReportFiles {
		path := filepath.Join(dir, name)
		fi, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("stat %s: %w", name, err)
		}
		if !fi.Mode().IsRegular() {
			return Manifest{}, fmt.Errorf("report file %s is not a regular file", name)
		}
		if fi.Size() > MaxTransferFileSize {
			return Manifest{}, fmt.Errorf("report file %s is %d bytes, over the %d-byte per-file cap",
				name, fi.Size(), MaxTransferFileSize)
		}
		sum, err := fileDigest(path)
		if err != nil {
			return Manifest{}, err
		}
		m.Files = append(m.Files, TransferFile{Name: name, Size: fi.Size(), SHA256: sum})
		m.TotalSize += fi.Size()
	}
	if m.TotalSize > MaxTransferTotal {
		return Manifest{}, fmt.Errorf("report set is %d bytes, over the %d-byte total cap",
			m.TotalSize, MaxTransferTotal)
	}
	return m, nil
}

// validateManifest enforces the receive-side contract before any bytes are
// accepted: every name allowlisted and traversal-free, every declared size
// within the per-file cap, and the total within the total cap. It is the
// receiver's gate; the sender's BuildManifest also enforces the caps so a
// well-behaved sender never trips it.
func validateManifest(m Manifest) error {
	var total int64
	for _, f := range m.Files {
		if !SafeReportName(f.Name) {
			return fmt.Errorf("rejected report file name %q", f.Name)
		}
		if f.Size < 0 || f.Size > MaxTransferFileSize {
			return fmt.Errorf("report file %s declares %d bytes, over the %d-byte per-file cap",
				f.Name, f.Size, MaxTransferFileSize)
		}
		total += f.Size
	}
	if total > MaxTransferTotal {
		return fmt.Errorf("manifest totals %d bytes, over the %d-byte total cap", total, MaxTransferTotal)
	}
	return nil
}

// fileDigest returns the hex-encoded SHA-256 of the file at path.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
