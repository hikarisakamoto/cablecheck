package reporting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
)

// ReceiverChannel is the narrow seam ReceiveReports drives: it receives the
// manifest and the chunk stream, and sends the per-file (and manifest-level
// decline) acks. The peer layer implements it by routing incoming
// report/report_chunk frames and marshaling report_ack frames.
type ReceiverChannel interface {
	// RecvFrame blocks for the next transfer frame. isManifest reports which
	// of the two values is meaningful: the Manifest (the opening frame) or the
	// ChunkFrame (all subsequent frames).
	RecvFrame(ctx context.Context) (m Manifest, c ChunkFrame, isManifest bool, err error)
	// SendAck sends one ack (per file, or a manifest-level decline).
	SendAck(ctx context.Context, a AckFrame) error
}

// ReceiveReports consumes a report transfer into dir. It validates the opening
// manifest against the allowlist and caps; on rejection it declines and
// returns nil (a declined transfer is not a receive error). It then writes
// each file to <name>.part while folding an incremental SHA-256, verifies the
// digest against the manifest on the last chunk, renames the .part to its
// final name on a match, and acks per file. A digest mismatch, offset gap or
// bad chunk name deletes the .part, acks !OK, and lets the sender retry — the
// receiver keeps NOTHING partial. Returns an error only for a channel or
// filesystem failure that prevents further progress, never for a per-file
// rejection.
func ReceiveReports(ctx context.Context, dir string, ch ReceiverChannel) error {
	m, _, isManifest, err := ch.RecvFrame(ctx)
	if err != nil {
		return fmt.Errorf("receive manifest: %w", err)
	}
	if !isManifest {
		return errors.New("reporting: first transfer frame was not a manifest")
	}
	if verr := validateManifest(m); verr != nil {
		if aerr := ch.SendAck(ctx, AckFrame{Declined: true, Error: verr.Error()}); aerr != nil {
			return fmt.Errorf("send manifest decline: %w", aerr)
		}
		return nil
	}
	if aerr := ensureWritableDir(dir); aerr != nil {
		if serr := ch.SendAck(ctx, AckFrame{Declined: true, Error: aerr.Error()}); serr != nil {
			return fmt.Errorf("send manifest decline: %w", serr)
		}
		return nil
	}
	for _, f := range m.Files {
		if err := receiveOneFile(ctx, dir, f, ch); err != nil {
			return err
		}
	}
	return nil
}

// receiveOneFile writes the manifest file f's chunk stream, tolerating the
// sender's one retry. The sender streams a whole file and then reads exactly
// one ack per attempt, so the receiver must ack at most once per attempt:
// after rejecting a chunk it stops writing and silently drains the failed
// attempt's remaining stragglers until the sender restarts the file at seq 0
// offset 0 (a fresh attempt) — re-acking each straggler would count chunks as
// attempts and desync the sender's one-ack-per-attempt protocol. A fresh
// .part is opened at each restart, discarding any partial bytes.
//
// The loop returns nil when the file is acked OK, and also when the sender's
// retry budget is exhausted (maxFileRetries+1 rejected attempts). At that point
// the two sides are NOT in lockstep: SendReports (transfer_send.go) aborts the
// ENTIRE transfer on the first file whose retries exhaust — it never streams a
// later file — so returning nil here lets ReceiveReports advance to the next
// manifest entry and park in RecvFrame. Convergence comes not from the sender
// re-streaming but from the session layer: PC1's subsequent complete frame
// tears the session down, which unblocks that parked RecvFrame with
// ErrSessionClosed. Other packages build on this exact cross-side contract, so
// it must be described as it actually behaves. Nothing partial is kept for a
// file that never verified.
func receiveOneFile(ctx context.Context, dir string, f TransferFile, ch ReceiverChannel) error {
	var w *partWriter
	cleanup := func() {
		if w != nil {
			w.abort()
			w = nil
		}
	}
	// draining is set after a rejection: the current attempt is doomed, so its
	// remaining stragglers are swallowed without acking or writing until the
	// next restart begins a fresh attempt.
	draining := false
	rejections := 0
	for {
		_, c, isManifest, err := ch.RecvFrame(ctx)
		if err != nil {
			cleanup()
			return fmt.Errorf("receive chunk: %w", err)
		}
		if isManifest {
			cleanup()
			return errors.New("reporting: unexpected manifest mid-transfer")
		}
		// A chunk at seq 0 offset 0 begins (or restarts, on retry) the file: it
		// ends any drain and opens a fresh .part, discarding prior partial bytes.
		restart := c.Seq == 0 && c.Offset == 0
		if draining && !restart {
			continue // straggler of the already-rejected attempt: swallow it
		}
		if c.Name != f.Name || !SafeReportName(c.Name) {
			// A chunk for a file this iteration is not expecting (or a rejected
			// name) is a protocol fault, not a per-file rejection.
			cleanup()
			if aerr := ch.SendAck(ctx, AckFrame{Name: c.Name, Error: "unexpected or rejected file name"}); aerr != nil {
				return fmt.Errorf("send reject ack: %w", aerr)
			}
			return fmt.Errorf("reporting: expected chunks for %s, got %q", f.Name, c.Name)
		}
		if restart {
			cleanup()
			draining = false
			nw, oerr := newPartWriter(dir, c.Name)
			if oerr != nil {
				return declineOnLocalFailure(ctx, ch, fmt.Errorf("open part file for %s: %w", c.Name, oerr))
			}
			w = nw
		}
		// reject acks the failed attempt exactly once, then enters drain mode;
		// it reports whether the sender still has retries left.
		reject := func(reason string) (exhausted bool, err error) {
			cleanup()
			draining = true
			rejections++
			if aerr := ch.SendAck(ctx, AckFrame{Name: f.Name, Error: reason}); aerr != nil {
				return false, fmt.Errorf("send reject ack: %w", aerr)
			}
			return rejections > maxFileRetries, nil
		}
		if w == nil || c.Offset != w.written {
			done, rerr := reject(fmt.Sprintf("offset gap: got %d", c.Offset))
			if rerr != nil {
				return rerr
			}
			if done {
				return nil
			}
			continue
		}
		if werr := w.write(c.Data); werr != nil {
			cleanup()
			return declineOnLocalFailure(ctx, ch, fmt.Errorf("write part for %s: %w", c.Name, werr))
		}
		if !c.Last {
			continue
		}
		// Last chunk: verify the whole-file digest and size before commit.
		gotSum := hex.EncodeToString(w.digest())
		if w.written != f.Size || gotSum != f.SHA256 {
			done, rerr := reject("sha256 mismatch")
			if rerr != nil {
				return rerr
			}
			if done {
				return nil
			}
			continue
		}
		if cerr := w.commit(); cerr != nil {
			cleanup()
			return declineOnLocalFailure(ctx, ch, fmt.Errorf("commit %s: %w", c.Name, cerr))
		}
		w = nil
		if aerr := ch.SendAck(ctx, AckFrame{Name: f.Name, OK: true}); aerr != nil {
			return fmt.Errorf("send ok ack for %s: %w", f.Name, aerr)
		}
		return nil
	}
}

// declineOnLocalFailure sends a manifest-level decline ack before returning the
// local filesystem failure that caused it. Without the decline the sender would
// block forever in RecvAck (its only unblock conditions are a routed ack or
// session teardown), wedging both unattended sessions when e.g. the receiver
// hits ENOSPC mid-transfer — a failure ensureWritableDir cannot pre-detect. The
// sender treats a decline as a clean, non-failure end of the whole transfer
// (errManifestDeclined). The original cause is still returned so the receiver's
// caller logs it as the by-contract transfer warning; a follow-up ack send
// error is joined so neither cause is lost.
func declineOnLocalFailure(ctx context.Context, ch ReceiverChannel, cause error) error {
	if aerr := ch.SendAck(ctx, AckFrame{Declined: true, Error: cause.Error()}); aerr != nil {
		return errors.Join(cause, fmt.Errorf("send local-failure decline: %w", aerr))
	}
	return cause
}

// partWriter streams a file's chunks into <name>.part while folding an
// incremental SHA-256; commit renames it to the final name, abort deletes it.
type partWriter struct {
	dir      string
	name     string
	partPath string
	f        *os.File
	h        hash.Hash
	written  int64
}

// newPartWriter creates (truncating) <dir>/<name>.part.
func newPartWriter(dir, name string) (*partWriter, error) {
	partPath := filepath.Join(dir, name+".part")
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &partWriter{dir: dir, name: name, partPath: partPath, f: f, h: sha256.New()}, nil
}

// write appends data to the part file and the running digest.
func (w *partWriter) write(data []byte) error {
	if _, err := w.f.Write(data); err != nil {
		return err
	}
	w.h.Write(data)
	w.written += int64(len(data))
	return nil
}

// digest returns the running SHA-256 sum.
func (w *partWriter) digest() []byte { return w.h.Sum(nil) }

// commit fsync-closes the part file and renames it to its final name.
func (w *partWriter) commit() error {
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	return os.Rename(w.partPath, filepath.Join(w.dir, w.name))
}

// abort closes and removes the part file, keeping nothing partial on disk.
func (w *partWriter) abort() {
	w.f.Close()
	_ = os.Remove(w.partPath)
}

// ensureWritableDir confirms dir exists and is a directory the receiver can
// write into, so a manifest is declined cleanly rather than failing mid-file.
func ensureWritableDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("report directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("report path %s is not a directory", dir)
	}
	return nil
}
