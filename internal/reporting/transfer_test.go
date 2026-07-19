package reporting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cablecheck/internal/testutil"
)

// pipeChannel is an in-memory sender<->receiver seam for the transfer tests.
// It carries the three transfer frame kinds (manifest, chunk, ack) as typed
// values over two channels — the reporting layer never sees protocol
// envelopes, so the package stays protocol-free (purity test).
type pipeChannel struct {
	toRecv chan transferFrame // sender -> receiver: manifest, chunks
	toSend chan AckFrame      // receiver -> sender: acks
	// mangle, when set, rewrites a chunk's data on its way to the receiver;
	// it models the app-level mangleReportChunk corruption hook.
	mangle func(seq int, data []byte) []byte
	// drop, when set and returning true, silently swallows a chunk (the sender
	// still advances) — it models a routed-frame loss or dedup that opens an
	// offset gap mid-file. The offset lets a test target a specific chunk.
	drop func(seq int, offset int64) bool
	// rewrite, when set, may replace a chunk on its way to the receiver: a
	// buggy/hostile sender that renames a chunk or inflates its Data past the
	// manifest-declared size. Returning the chunk unchanged is a no-op.
	rewrite func(c ChunkFrame) ChunkFrame
}

// transferFrame is one manifest-or-chunk value on the wire between halves.
type transferFrame struct {
	manifest *Manifest
	chunk    *ChunkFrame
}

func newPipeChannel() *pipeChannel {
	return &pipeChannel{
		toRecv: make(chan transferFrame, 8),
		toSend: make(chan AckFrame, 8),
	}
}

// senderEnd adapts the pipe for the SendReports half.
type senderEnd struct{ p *pipeChannel }

func (s senderEnd) SendManifest(ctx context.Context, m Manifest) error {
	select {
	case s.p.toRecv <- transferFrame{manifest: &m}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s senderEnd) SendChunk(ctx context.Context, c ChunkFrame) error {
	if s.p.drop != nil && s.p.drop(c.Seq, c.Offset) {
		return nil // dropped in flight; the sender still advances its offset
	}
	if s.p.mangle != nil {
		c.Data = s.p.mangle(c.Seq, append([]byte(nil), c.Data...))
	}
	if s.p.rewrite != nil {
		c = s.p.rewrite(c)
	}
	select {
	case s.p.toRecv <- transferFrame{chunk: &c}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s senderEnd) RecvAck(ctx context.Context) (AckFrame, error) {
	select {
	case a := <-s.p.toSend:
		return a, nil
	case <-ctx.Done():
		return AckFrame{}, ctx.Err()
	}
}

// receiverEnd adapts the pipe for the ReceiveReports half.
type receiverEnd struct{ p *pipeChannel }

func (r receiverEnd) RecvFrame(ctx context.Context) (Manifest, ChunkFrame, bool, error) {
	select {
	case f := <-r.p.toRecv:
		if f.manifest != nil {
			return *f.manifest, ChunkFrame{}, true, nil
		}
		return Manifest{}, *f.chunk, false, nil
	case <-ctx.Done():
		return Manifest{}, ChunkFrame{}, false, ctx.Err()
	}
}

func (r receiverEnd) SendAck(ctx context.Context, a AckFrame) error {
	select {
	case r.p.toSend <- a:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// writeReportSet writes the three allowlisted report files into dir with the
// given bodies and returns their sha256 digests keyed by name.
func writeReportSet(t *testing.T, dir string, bodies map[string]string) map[string]string {
	t.Helper()
	digests := map[string]string{}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		sum := sha256.Sum256([]byte(body))
		digests[name] = hex.EncodeToString(sum[:])
	}
	return digests
}

// runTransfer runs both halves concurrently over one pipe and returns their
// errors once both finish.
func runTransfer(t *testing.T, p *pipeChannel, srcDir, dstDir string) (sendErr, recvErr error) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		recvErr = ReceiveReports(context.Background(), dstDir, receiverEnd{p})
	}()
	go func() {
		defer wg.Done()
		sendErr = SendReports(context.Background(), srcDir, senderEnd{p})
	}()
	wg.Wait()
	return sendErr, recvErr
}

// TestTransferSHA256HappyPath sends the full report set and asserts the
// receiver's copies are byte-identical (digest-equal) to the source, with no
// .part files left behind.
func TestTransferSHA256HappyPath(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	// One file spans several chunks so the chunking path is exercised.
	big := strings.Repeat("cablecheck-report-payload-", 40000) // > ChunkSize
	bodies := map[string]string{
		"report.json": `{"schemaVersion":"1.0.0"}`,
		"report.md":   "# report\n",
		"summary.txt": big,
	}
	digests := writeReportSet(t, srcDir, bodies)

	p := newPipeChannel()
	sendErr, recvErr := runTransferBounded(t, p, srcDir, dstDir)
	if sendErr != nil {
		t.Fatalf("SendReports: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("ReceiveReports: %v", recvErr)
	}

	for name, want := range digests {
		got, err := os.ReadFile(filepath.Join(dstDir, name))
		if err != nil {
			t.Fatalf("read received %s: %v", name, err)
		}
		sum := sha256.Sum256(got)
		if hex.EncodeToString(sum[:]) != want {
			t.Errorf("received %s digest mismatch", name)
		}
	}
	assertNoPartFiles(t, dstDir)
}

// TestTransferCorruptedChunk flips a byte in one chunk. The receiver keeps
// NOTHING partial for that file, its ack is !OK, and the sender's single retry
// (which is not corrupted) then succeeds so the file lands intact.
func TestTransferCorruptedChunk(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	big := strings.Repeat("X", ChunkSize+1024) // two chunks
	bodies := map[string]string{"report.json": big}
	digests := writeReportSet(t, srcDir, bodies)

	p := newPipeChannel()
	var once sync.Once
	p.mangle = func(seq int, data []byte) []byte {
		// Corrupt only the very first chunk sent, once: the retry is clean.
		if seq == 0 && len(data) > 0 {
			var did bool
			once.Do(func() { data[0] ^= 0xFF; did = true })
			_ = did
		}
		return data
	}

	sendErr, recvErr := runTransferBounded(t, p, srcDir, dstDir)
	if sendErr != nil {
		t.Fatalf("SendReports: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("ReceiveReports: %v", recvErr)
	}

	// The retry landed the intact file.
	got, err := os.ReadFile(filepath.Join(dstDir, "report.json"))
	if err != nil {
		t.Fatalf("read received report.json: %v", err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != digests["report.json"] {
		t.Errorf("report.json not intact after retry")
	}
	assertNoPartFiles(t, dstDir)
}

// TestTransferDroppedChunkRetries drops one middle chunk of a multi-chunk
// file's first attempt, opening an offset gap. Per the one-ack-per-attempt
// contract the receiver must ack that attempt at most once (silently draining
// the remaining stragglers of the failed attempt), let the sender retry once,
// and land the intact file on the clean retry — not consume the retry budget
// by acking every straggler chunk.
func TestTransferDroppedChunkRetries(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	// Four chunks: 3 full + a trailing partial, so a dropped seq 1 leaves two
	// more stragglers (seq 2, seq 3) in the same failed attempt.
	big := strings.Repeat("Z", 3*ChunkSize+512)
	bodies := map[string]string{"report.json": big}
	digests := writeReportSet(t, srcDir, bodies)

	p := newPipeChannel()
	var once sync.Once
	p.drop = func(seq int, offset int64) bool {
		if seq == 1 {
			var did bool
			once.Do(func() { did = true })
			return did // drop seq 1 of the first attempt only; the retry is clean
		}
		return false
	}

	sendErr, recvErr := runTransferBounded(t, p, srcDir, dstDir)
	if sendErr != nil {
		t.Fatalf("SendReports: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("ReceiveReports: %v", recvErr)
	}

	got, err := os.ReadFile(filepath.Join(dstDir, "report.json"))
	if err != nil {
		t.Fatalf("read received report.json: %v", err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != digests["report.json"] {
		t.Errorf("report.json not intact after a dropped-chunk retry")
	}
	assertNoPartFiles(t, dstDir)
}

// TestTransferCorruptedChunkNoRetryLandsNothing corrupts every attempt: after
// the one allowed retry also fails, the sender reports a transfer failure and
// the receiver keeps nothing on disk (no final file, no .part).
func TestTransferCorruptedChunkNoRetryLandsNothing(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	bodies := map[string]string{"report.json": strings.Repeat("Y", 4096)}
	writeReportSet(t, srcDir, bodies)

	p := newPipeChannel()
	p.mangle = func(seq int, data []byte) []byte {
		if len(data) > 0 {
			data[0] ^= 0xFF // always corrupt
		}
		return data
	}

	sendErr, recvErr := runTransferBounded(t, p, srcDir, dstDir)
	if sendErr == nil {
		t.Errorf("SendReports err = nil, want a transfer failure after the retry")
	}
	if recvErr != nil {
		t.Fatalf("ReceiveReports: %v", recvErr)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "report.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("report.json exists after two failed transfers, want nothing kept")
	}
	assertNoPartFiles(t, dstDir)
}

// TestTransferOversizeRejected rejects a file over the per-file cap and a
// manifest over the total cap, at manifest-build time (per file) and at
// receive time (both).
func TestTransferOversizeRejected(t *testing.T) {
	t.Run("per-file-at-build", func(t *testing.T) {
		srcDir := t.TempDir()
		oversize := make([]byte, MaxTransferFileSize+1)
		if err := os.WriteFile(filepath.Join(srcDir, "report.json"), oversize, 0o600); err != nil {
			t.Fatalf("write oversize: %v", err)
		}
		if _, err := BuildManifest(srcDir); err == nil {
			t.Errorf("BuildManifest err = nil, want a per-file cap rejection")
		}
	})

	t.Run("per-file-at-receive", func(t *testing.T) {
		dstDir := t.TempDir()
		m := Manifest{
			Files:     []TransferFile{{Name: "report.json", Size: MaxTransferFileSize + 1, SHA256: "00"}},
			TotalSize: MaxTransferFileSize + 1,
		}
		if err := validateManifest(m); err == nil {
			t.Errorf("validateManifest err = nil, want a per-file cap rejection")
		}
		_ = dstDir
	})

	t.Run("total-at-receive", func(t *testing.T) {
		big := int64(MaxTransferFileSize)
		m := Manifest{
			Files: []TransferFile{
				{Name: "report.json", Size: big, SHA256: "00"},
				{Name: "report.md", Size: big, SHA256: "00"},
				{Name: "summary.txt", Size: big, SHA256: "00"},
			},
			TotalSize: 3 * big,
		}
		if err := validateManifest(m); err == nil {
			t.Errorf("validateManifest err = nil, want a total cap rejection")
		}
	})
}

// TestTransferNameAllowlist rejects any name outside the allowlist and any
// traversal name, both at validation time and when a chunk names a bad file.
func TestTransferNameAllowlist(t *testing.T) {
	bad := []string{
		"passwd", "report.txt", "config.json", "raw/x.log",
		"../report.json", "..\\report.json", "a/../b", "./report.json",
		"/etc/passwd", "report.json/..", "", ".", "..",
	}
	for _, name := range bad {
		if SafeReportName(name) {
			t.Errorf("SafeReportName(%q) = true, want false", name)
		}
		m := Manifest{Files: []TransferFile{{Name: name, Size: 1, SHA256: "00"}}, TotalSize: 1}
		if err := validateManifest(m); err == nil {
			t.Errorf("validateManifest accepted bad name %q", name)
		}
	}
	for _, name := range []string{"report.json", "report.md", "summary.txt"} {
		if !SafeReportName(name) {
			t.Errorf("SafeReportName(%q) = false, want true", name)
		}
	}
}

// TestTransferReceiverLocalFailureDoesNotHang exercises the wedge described in
// the T14 review: a receiver-side local filesystem failure (here, an open
// failure because the report directory is read-only) must not leave the sender
// blocked forever in RecvAck. The receiver declines the transfer so the sender
// unblocks cleanly and treats it as a non-failure, and nothing partial is kept.
func TestTransferReceiverLocalFailureDoesNotHang(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions; the open failure cannot be provoked")
	}
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	// A multi-chunk file makes the read-only open the first thing that fails.
	bodies := map[string]string{"report.json": strings.Repeat("Q", ChunkSize+7)}
	writeReportSet(t, srcDir, bodies)

	// Make the destination read-only so newPartWriter's O_CREATE|O_WRONLY open
	// fails while ensureWritableDir (a bare Stat) still passes.
	if err := os.Chmod(dstDir, 0o555); err != nil {
		t.Fatalf("chmod dst: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dstDir, 0o755) })

	p := newPipeChannel()
	sendErr, recvErr := runTransferBounded(t, p, srcDir, dstDir)

	// The sender must not hang; a declined transfer is not a sender failure.
	if sendErr != nil {
		t.Errorf("SendReports err = %v, want nil (a decline is not a failure)", sendErr)
	}
	// A local FS failure is a receive error the caller logs as a warning.
	if recvErr == nil {
		t.Errorf("ReceiveReports err = nil, want a local-failure error")
	}

	// Nothing landed: no final file, no .part scratch.
	if err := os.Chmod(dstDir, 0o755); err != nil {
		t.Fatalf("restore dst perms: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "report.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("report.json exists after a receiver local failure, want nothing kept")
	}
	assertNoPartFiles(t, dstDir)
}

// TestTransferOversizeChunkRejectedMidStream pins T1a: a manifest declaring a
// small file followed by a contiguous chunk stream that would exceed the
// declared size must be rejected BEFORE the last chunk — a token-authed but
// buggy/hostile sender must not be able to stream unlimited non-Last bytes past
// the manifest-declared (cap-validated) size and fill the receiver's disk. The
// receiver keeps nothing, and never writes more than the declared size.
func TestTransferOversizeChunkRejectedMidStream(t *testing.T) {
	dstDir := t.TempDir()

	// Manifest declares 4 bytes; the sender streams a 4-byte first chunk (Last
	// = false) then keeps going. The receiver must reject the moment the
	// running total would exceed 4, not wait for a Last that never comes.
	const declared = 4
	m := Manifest{
		Files:     []TransferFile{{Name: "report.json", Size: declared, SHA256: "00"}},
		TotalSize: declared,
	}

	// hostileEnd streams the manifest, then contiguous non-Last chunks forever
	// (until the receiver declines) each carrying `declared` bytes at the
	// running offset — the classic disk-fill attack. maxPart records the
	// largest .part size the receiver ever flushed, sampled just before each
	// ack: the receiver must reject before its running total passes `declared`,
	// so this can never exceed `declared`.
	acks := make(chan AckFrame, 1)
	var maxPart int64
	sampleWritten := func() {
		// Sampled just before each new chunk is yielded: at that point the
		// receiver has fully processed (and flushed) all prior chunks, so the
		// .part size reflects the running written total. With the fix the
		// receiver rejects before ever writing past the declared size.
		if fi, err := os.Stat(filepath.Join(dstDir, "report.json.part")); err == nil {
			if fi.Size() > maxPart {
				maxPart = fi.Size()
			}
		}
	}
	ch := &scriptedRecvChannel{
		frames: []scriptedFrame{{m: m, isManifest: true}},
		next: func(seq int) scriptedFrame {
			sampleWritten()
			return scriptedFrame{c: ChunkFrame{
				Name:   "report.json",
				Seq:    seq,
				Offset: int64(seq) * 4,
				Data:   []byte("data"), // 4 bytes
				Last:   false,
			}}
		},
		onAck: func(a AckFrame) {
			select {
			case acks <- a:
			default:
			}
		},
	}

	done := make(chan error, 1)
	go func() { done <- ReceiveReports(context.Background(), dstDir, ch) }()

	var ack AckFrame
	select {
	case ack = <-acks:
	case <-time.After(testutil.TestTimeout(t)):
		t.Fatal("receiver never rejected the oversize stream (disk-fill wedge)")
	}
	select {
	case <-done:
	case <-time.After(testutil.TestTimeout(t)):
		t.Fatal("ReceiveReports did not return after rejecting the oversize stream")
	}

	// The reject is a real rejection (not an OK ack).
	if ack.OK {
		t.Errorf("ack = %+v, want a rejection of the oversize stream", ack)
	}
	// Nothing oversized was ever committed, and no .part scratch survived.
	if _, err := os.Stat(filepath.Join(dstDir, "report.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("report.json exists after an oversize stream, want nothing kept")
	}
	if maxPart > declared {
		t.Errorf("receiver flushed %d bytes to disk, over the declared %d-byte size", maxPart, declared)
	}
	assertNoPartFiles(t, dstDir)
}

// TestTransferUnexpectedChunkNameFatal pins T1b: a manifest naming report.json
// followed by a first chunk naming a different (still-allowlisted) file is a
// FATAL, non-retryable decline — the sender must STOP rather than retransmit
// into a receiver that has already gone away. The ack carries Declined so the
// sender short-circuits (errManifestDeclined) instead of treating it as a
// retryable !OK and parking forever in RecvAck; the receiver keeps nothing.
func TestTransferUnexpectedChunkNameFatal(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	// A single-chunk report.json is enough; the sender renames the chunk.
	bodies := map[string]string{"report.json": "hello"}
	writeReportSet(t, srcDir, bodies)

	p := newPipeChannel()
	// The manifest still names report.json (BuildManifest only offers
	// allowlisted files), but every chunk claims report.md — an unexpected name
	// for the file the receiver is currently expecting.
	p.rewrite = func(c ChunkFrame) ChunkFrame {
		c.Name = "report.md"
		return c
	}

	sendErr, recvErr := runTransferBounded(t, p, srcDir, dstDir)

	// The sender must not park in RecvAck: a fatal decline ends the transfer as
	// a non-failure (errManifestDeclined -> nil), NOT a retry loop or a hang.
	if sendErr != nil {
		t.Errorf("SendReports err = %v, want nil (a fatal decline is not a failure)", sendErr)
	}
	// The receiver surfaces the protocol fault as a receive error the caller
	// logs as a warning.
	if recvErr == nil {
		t.Errorf("ReceiveReports err = nil, want a protocol-fault error for the unexpected chunk name")
	}
	// Nothing landed under either name.
	for _, name := range []string{"report.json", "report.md"} {
		if _, err := os.Stat(filepath.Join(dstDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after an unexpected-name chunk, want nothing kept", name)
		}
	}
	assertNoPartFiles(t, dstDir)
}

// runTransferBounded runs both transfer halves concurrently and fails the test
// (rather than wedging the whole `go test` run to the global timeout) if either
// half has not returned within the testutil.TestTimeout budget — the watchdog
// that turns a future mutual-deadlock regression into a fast, useful failure.
func runTransferBounded(t *testing.T, p *pipeChannel, srcDir, dstDir string) (sendErr, recvErr error) {
	t.Helper()
	limit := testutil.TestTimeout(t)
	done := make(chan struct{})
	go func() {
		sendErr, recvErr = runTransfer(t, p, srcDir, dstDir)
		close(done)
	}()
	select {
	case <-done:
		return sendErr, recvErr
	case <-time.After(limit):
		t.Fatalf("transfer did not complete within %s (mutual hang)", limit)
		return nil, nil
	}
}

// scriptedFrame is one manifest-or-chunk value a scriptedRecvChannel yields.
type scriptedFrame struct {
	m          Manifest
	c          ChunkFrame
	isManifest bool
}

// scriptedRecvChannel is a ReceiverChannel that drives ReceiveReports from a
// fixed prologue of frames and then an unbounded generated chunk stream — the
// seam a hostile-sender test needs, since the pipeChannel's SendReports half
// can only ever offer well-behaved, manifest-consistent chunks. It captures
// every ack and lets the test sample disk state just before each ack.
type scriptedRecvChannel struct {
	frames []scriptedFrame             // fixed prologue (manifest, seed chunks)
	next   func(seq int) scriptedFrame // generator for chunks past the prologue
	onAck  func(a AckFrame)
	seq    int
}

func (s *scriptedRecvChannel) RecvFrame(ctx context.Context) (Manifest, ChunkFrame, bool, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, ChunkFrame{}, false, err
	}
	if len(s.frames) > 0 {
		f := s.frames[0]
		s.frames = s.frames[1:]
		return f.m, f.c, f.isManifest, nil
	}
	f := s.next(s.seq)
	s.seq++
	return f.m, f.c, f.isManifest, nil
}

func (s *scriptedRecvChannel) SendAck(ctx context.Context, a AckFrame) error {
	if s.onAck != nil {
		s.onAck(a)
	}
	return nil
}

// assertNoPartFiles fails if any *.part scratch file survived in dir.
func assertNoPartFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("leftover partial file %q in %s", e.Name(), dir)
		}
	}
}
