package reporting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SenderChannel is the narrow seam SendReports drives: it sends the manifest
// and the per-file chunk stream, and receives the per-file acks. The peer
// layer implements it by marshaling to protocol report/report_chunk frames and
// routing report_ack frames back. Keeping the interface here — rather than
// referencing protocol types — is what lets the reporting package stay
// protocol-free.
type SenderChannel interface {
	// SendManifest sends the opening manifest.
	SendManifest(ctx context.Context, m Manifest) error
	// SendChunk sends one file chunk.
	SendChunk(ctx context.Context, c ChunkFrame) error
	// RecvAck blocks for the next ack from the receiver.
	RecvAck(ctx context.Context) (AckFrame, error)
}

// maxFileRetries is the number of extra attempts per file after the first: one
// retry, per the transfer contract (docs/design/proto.md §8 step 4).
const maxFileRetries = 1

// SendReports streams the allowlisted report files in dir to the receiver over
// ch: it builds and sends the manifest, then streams each file in 256 KiB
// chunks and waits for that file's ack before the next. A file whose ack is
// !OK is retried once; a second failure aborts the transfer with an error.
//
// The returned error is advisory only in the caller's contract — a report
// transfer failure is a warning that never changes classification or exit
// code — but SendReports still reports it so the caller can log the warning.
// A manifest-level decline (the receiver refusing the whole set) is NOT an
// error: the sender simply has nothing more to do.
func SendReports(ctx context.Context, dir string, ch SenderChannel) error {
	m, err := BuildManifest(dir)
	if err != nil {
		return fmt.Errorf("build report manifest: %w", err)
	}
	if err := ch.SendManifest(ctx, m); err != nil {
		return fmt.Errorf("send report manifest: %w", err)
	}
	for _, f := range m.Files {
		if err := sendOneFile(ctx, dir, f, ch); err != nil {
			if errors.Is(err, errManifestDeclined) {
				return nil // the receiver declined the whole set; not a failure
			}
			return err
		}
	}
	return nil
}

// errManifestDeclined signals a manifest-level decline observed while sending
// a file's chunks (the receiver acks the empty name with Declined).
var errManifestDeclined = errors.New("reporting: receiver declined the report transfer")

// sendOneFile streams f's bytes as chunks and awaits its ack, retrying once on
// a failed ack. A decline (empty ack name, Declined set) short-circuits with
// errManifestDeclined.
func sendOneFile(ctx context.Context, dir string, f TransferFile, ch SenderChannel) error {
	var lastErr error
	for attempt := 0; attempt <= maxFileRetries; attempt++ {
		if err := streamFileChunks(ctx, dir, f, ch); err != nil {
			return fmt.Errorf("stream %s: %w", f.Name, err)
		}
		ack, err := ch.RecvAck(ctx)
		if err != nil {
			return fmt.Errorf("await ack for %s: %w", f.Name, err)
		}
		if ack.Declined {
			return errManifestDeclined
		}
		if ack.OK && ack.Name == f.Name {
			return nil
		}
		lastErr = fmt.Errorf("receiver rejected %s: %s", f.Name, ack.Error)
	}
	return lastErr
}

// streamFileChunks reads f from dir and sends it as contiguous 256 KiB chunks,
// each carrying the running offset so the receiver can detect gaps. A file
// shorter than one chunk still sends a single Last chunk (possibly empty).
func streamFileChunks(ctx context.Context, dir string, f TransferFile, ch SenderChannel) error {
	file, err := os.Open(filepath.Join(dir, f.Name))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer file.Close()

	buf := make([]byte, ChunkSize)
	var offset int64
	seq := 0
	for {
		n, rerr := io.ReadFull(file, buf)
		last := false
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			last = true
		} else if rerr != nil {
			return fmt.Errorf("read: %w", rerr)
		}
		// A file whose length is an exact multiple of ChunkSize returns
		// (ChunkSize, nil) then (0, io.EOF): the trailing empty read marks the
		// last chunk without duplicating data.
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := ChunkFrame{
			Name:   f.Name,
			Seq:    seq,
			Offset: offset,
			Data:   append([]byte(nil), buf[:n]...),
			Last:   last,
		}
		if err := ch.SendChunk(ctx, chunk); err != nil {
			return fmt.Errorf("send chunk %d: %w", seq, err)
		}
		offset += int64(n)
		seq++
		if last {
			return nil
		}
	}
}
