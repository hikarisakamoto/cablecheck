package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cablecheck/internal/clock"
)

// killPollInterval is how often KillAll re-checks whether terminated
// processes have actually exited during the grace period.
const killPollInterval = 50 * time.Millisecond

// sessionFileName is the per-session liveness marker inside each testID
// directory: the ProcessInfo of the cablecheck process that owns the session.
// ScanStale skips any directory whose marker still ownership-verifies, so
// concurrent cablecheck sessions never mistake each other's live children
// for stale survivors.
const sessionFileName = "session.json"

// ProcessInfo identifies one session-owned external process precisely enough
// to defeat PID reuse: a signal is only ever sent after re-verifying the
// /proc starttime and argv[0] against this record.
type ProcessInfo struct {
	// PID is the process id.
	PID int `json:"pid"`
	// PGID is the process group id (== PID for runner-started children).
	PGID int `json:"pgid"`
	// StartTicks is the process start time in clock ticks since boot,
	// from /proc/<pid>/stat field 22 (parsed after the last ')').
	StartTicks uint64 `json:"startTicks"`
	// Argv0 is the expected basename of the executable, e.g. "iperf3".
	Argv0 string `json:"argv0"`
	// Label is the human-readable slug the process was started under.
	Label string `json:"label"`
	// TestID is the cablecheck session that owns the process.
	TestID string `json:"testID"`
}

// DefaultBaseDir returns the root of the pidfile state tree:
// ${XDG_RUNTIME_DIR:-/tmp}/cablecheck.
func DefaultBaseDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = "/tmp"
	}
	return filepath.Join(base, "cablecheck")
}

// Registry tracks the external processes owned by one test session and
// persists a pidfile per process so a later cablecheck run can detect and
// clean up stale survivors. Never use pkill/killall: every kill path here is
// ownership-verified. Concurrent cablecheck sessions are safe: each session
// directory carries a liveness marker (the owning process's own ProcessInfo)
// that ScanStale verifies before treating anything in the directory as stale.
type Registry struct {
	mu       sync.Mutex
	procs    map[int]ProcessInfo
	stateDir string
	clk      clock.Clock
	session  ProcessInfo // this process's identity, persisted as the liveness marker
}

// NewRegistry creates (if needed) the state directory
// ${XDG_RUNTIME_DIR:-/tmp}/cablecheck/<testID>/, writes the session-liveness
// marker identifying the calling process, and returns a Registry rooted
// there. clk paces KillAll's SIGTERM-to-SIGKILL grace wait; production
// callers pass clock.Real{}.
func NewRegistry(testID string, clk clock.Clock) (*Registry, error) {
	if testID == "" || testID != filepath.Base(testID) || testID == "." || testID == ".." {
		return nil, fmt.Errorf("runner: invalid registry test id %q", testID)
	}
	session, err := NewProcessInfo(os.Getpid(), "cablecheck-session", testID)
	if err != nil {
		return nil, fmt.Errorf("runner: identify own process for session marker: %w", err)
	}
	r := &Registry{
		procs:    make(map[int]ProcessInfo),
		stateDir: filepath.Join(DefaultBaseDir(), testID),
		clk:      clk,
		session:  session,
	}
	if err := r.ensureStateDir(); err != nil {
		return nil, err
	}
	return r, nil
}

// ensureStateDir (re)creates the state directory and its session-liveness
// marker. Register re-runs it on every registration, so pidfile writes
// survive a sibling session's ScanStale pruning the directory in the narrow
// window before the marker exists.
func (r *Registry) ensureStateDir() error {
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return fmt.Errorf("runner: create registry state dir: %w", err)
	}
	marker := filepath.Join(r.stateDir, sessionFileName)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	data, err := json.MarshalIndent(r.session, "", "  ")
	if err != nil {
		return fmt.Errorf("runner: marshal session marker: %w", err)
	}
	if err := writeFileAtomic(marker, data); err != nil {
		return fmt.Errorf("runner: write session marker: %w", err)
	}
	return nil
}

// StateDir returns the directory holding this registry's pidfiles.
func (r *Registry) StateDir() string { return r.stateDir }

// Register records p and writes its pidfile (<pid>.json). The returned
// unregister function is idempotent; it forgets the process and removes the
// pidfile. Callers invoke it once the process has been waited for.
func (r *Registry) Register(p ProcessInfo) (func(), error) {
	if p.PID <= 0 {
		return nil, fmt.Errorf("runner: register: invalid pid %d", p.PID)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("runner: register: %w", err)
	}
	if err := r.ensureStateDir(); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(pidfilePath(r.stateDir, p.PID), data); err != nil {
		return nil, fmt.Errorf("runner: write pidfile: %w", err)
	}
	r.mu.Lock()
	r.procs[p.PID] = p
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { r.forget(p) })
	}, nil
}

// forget drops p from the in-memory table and removes its pidfile.
func (r *Registry) forget(p ProcessInfo) {
	r.mu.Lock()
	delete(r.procs, p.PID)
	r.mu.Unlock()
	os.Remove(pidfilePath(r.stateDir, p.PID))
}

// KillAll terminates every registered process whose ownership still
// verifies: SIGTERM to the negative pgid, a bounded grace wait (polling for
// actual exit), then SIGKILL to survivors. All entries and pidfiles are
// cleaned up afterwards. It returns the signalling errors encountered;
// ESRCH is never an error (the group may exit between check and signal).
func (r *Registry) KillAll(ctx context.Context) []error {
	r.mu.Lock()
	snapshot := make([]ProcessInfo, 0, len(r.procs))
	for _, p := range r.procs {
		snapshot = append(snapshot, p)
	}
	r.mu.Unlock()

	var errs []error
	var live []ProcessInfo
	for _, p := range snapshot {
		if !VerifyOwnership(p) {
			continue // already gone (or reused pid): never signal
		}
		if err := syscall.Kill(-p.PGID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("runner: SIGTERM pgid %d (%s): %w", p.PGID, p.Label, err))
		}
		live = append(live, p)
	}

	if len(live) > 0 {
		grace := r.clk.After(DefaultGracePeriod)
		ticker := r.clk.NewTicker(killPollInterval)
		defer ticker.Stop()
	waitLoop:
		for {
			var still []ProcessInfo
			for _, p := range live {
				if VerifyOwnership(p) {
					still = append(still, p)
				}
			}
			live = still
			if len(live) == 0 {
				break
			}
			select {
			case <-ctx.Done():
				break waitLoop
			case <-grace:
				break waitLoop
			case <-ticker.C():
			}
		}
		for _, p := range live {
			if !VerifyOwnership(p) {
				continue
			}
			if err := syscall.Kill(-p.PGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("runner: SIGKILL pgid %d (%s): %w", p.PGID, p.Label, err))
			}
		}
	}

	for _, p := range snapshot {
		r.forget(p)
	}
	return errs
}

// VerifyOwnership reports whether the live process at p.PID is still the
// process described by p: the /proc/<pid>/stat starttime must match
// StartTicks (defeats PID reuse), the /proc/<pid>/stat pgrp must match PGID
// (every kill path signals -PGID, so the group itself must verify — a stale
// or tampered PGID such as 0 would otherwise direct the signal at an
// unverified group, or with kill(-0) at the caller's own), and the
// /proc/<pid>/cmdline argv[0] basename must match Argv0. A missing process,
// a zombie (empty cmdline), or any read error verifies false.
func VerifyOwnership(p ProcessInfo) bool {
	st, err := readProcStat(p.PID)
	if err != nil || st.startTicks != p.StartTicks || st.pgrp != p.PGID {
		return false
	}
	argv0, err := readProcArgv0(p.PID)
	if err != nil {
		return false
	}
	return filepath.Base(argv0) == filepath.Base(p.Argv0)
}

// NewProcessInfo snapshots the identity of a live process from /proc so it
// can be registered and later re-verified before any signal.
func NewProcessInfo(pid int, label, testID string) (ProcessInfo, error) {
	st, err := readProcStat(pid)
	if err != nil {
		return ProcessInfo{}, err
	}
	argv0, err := readProcArgv0(pid)
	if err != nil {
		return ProcessInfo{}, err
	}
	return ProcessInfo{
		PID:        pid,
		PGID:       st.pgrp,
		StartTicks: st.startTicks,
		Argv0:      filepath.Base(argv0),
		Label:      label,
		TestID:     testID,
	}, nil
}

// ScanStale walks baseDir (one subdirectory per testID, each holding a
// session-liveness marker plus <pid>.json pidfiles) and returns the entries
// whose owning session is dead but whose processes are still alive and
// ownership-verified — true stale survivors of a crashed or killed run.
//
// Directories whose session marker still verifies against a live process
// belong to a concurrently running cablecheck session and are skipped
// untouched: their processes are NOT stale and must never be signalled. In
// dead-session directories, pidfiles for dead or unverifiable processes,
// unparseable pidfiles, the dead session marker, and stray temp files are
// removed silently; emptied testID directories are pruned. A missing baseDir
// yields (nil, nil).
func ScanStale(baseDir string) ([]ProcessInfo, error) {
	dirs, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("runner: scan stale state: %w", err)
	}
	var stale []ProcessInfo
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(baseDir, d.Name())
		if sessionAlive(dir) {
			continue // a live cablecheck session owns this dir: hands off
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			path := filepath.Join(dir, f.Name())
			if f.Name() == sessionFileName || !strings.HasSuffix(f.Name(), ".json") {
				os.Remove(path) // dead session marker or stray temp file
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var p ProcessInfo
			if err := json.Unmarshal(data, &p); err != nil || !VerifyOwnership(p) {
				os.Remove(path) // dead, reused, or corrupt: clean silently
				continue
			}
			stale = append(stale, p)
		}
		os.Remove(dir) // prunes only if now empty; error ignored
	}
	return stale, nil
}

// sessionAlive reports whether dir belongs to a currently running cablecheck
// session: its session marker parses and still ownership-verifies.
func sessionAlive(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, sessionFileName))
	if err != nil {
		return false
	}
	var p ProcessInfo
	if err := json.Unmarshal(data, &p); err != nil {
		return false
	}
	return VerifyOwnership(p)
}

// writeFileAtomic writes data to path via a same-directory temp file and
// rename, so a concurrent reader (a sibling session's ScanStale) can never
// observe a torn JSON file. Temp names carry no ".json" suffix and are
// therefore invisible to the pidfile scan.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// pidfilePath returns the pidfile location for pid under stateDir.
func pidfilePath(stateDir string, pid int) string {
	return filepath.Join(stateDir, strconv.Itoa(pid)+".json")
}

// procStat holds the fields cablecheck needs from /proc/<pid>/stat.
type procStat struct {
	pgrp       int
	startTicks uint64
}

// readProcStat parses /proc/<pid>/stat. The comm field (2) may contain
// spaces and parentheses, so parsing starts after the LAST ')': field 3
// (state) is then index 0, so overall field N lives at index N-3.
func readProcStat(pid int) (procStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procStat{}, fmt.Errorf("runner: read proc stat: %w", err)
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return procStat{}, fmt.Errorf("runner: malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[i+1:]))
	const (
		pgrpIdx  = 5 - 3  // stat field 5: pgrp
		startIdx = 22 - 3 // stat field 22: starttime
	)
	if len(fields) <= startIdx {
		return procStat{}, fmt.Errorf("runner: short /proc/%d/stat", pid)
	}
	pgrp, err := strconv.Atoi(fields[pgrpIdx])
	if err != nil {
		return procStat{}, fmt.Errorf("runner: parse pgrp: %w", err)
	}
	start, err := strconv.ParseUint(fields[startIdx], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("runner: parse starttime: %w", err)
	}
	return procStat{pgrp: pgrp, startTicks: start}, nil
}

// readProcArgv0 returns argv[0] from /proc/<pid>/cmdline. Zombies and kernel
// threads have an empty cmdline, which is an error here: such a process can
// no longer be positively identified.
func readProcArgv0(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", fmt.Errorf("runner: read proc cmdline: %w", err)
	}
	argv0, _, _ := bytes.Cut(data, []byte{0})
	if len(argv0) == 0 {
		return "", fmt.Errorf("runner: empty cmdline for pid %d (zombie or kernel thread)", pid)
	}
	return string(argv0), nil
}
