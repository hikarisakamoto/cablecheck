package testutil_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"cablecheck/internal/testutil"
)

// fakeCleanupTB captures Cleanup registrations and Errorf calls so the
// LeakCheck failure path can be exercised without failing the real test.
type fakeCleanupTB struct {
	testing.TB
	mu       sync.Mutex
	cleanups []func()
	errors   []string
}

func (f *fakeCleanupTB) Cleanup(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, fn)
}

func (f *fakeCleanupTB) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeCleanupTB) Helper() {}

// runCleanups runs registered cleanups in LIFO order like testing.T does.
func (f *fakeCleanupTB) runCleanups() {
	f.mu.Lock()
	cleanups := f.cleanups
	f.cleanups = nil
	f.mu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
}

func (f *fakeCleanupTB) errorCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.errors)
}

func TestLeakCheckPassesWhenGoroutinesExit(t *testing.T) {
	ft := &fakeCleanupTB{TB: t}
	testutil.LeakCheck(ft)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-stop
	}()
	close(stop)
	<-done
	ft.runCleanups()
	if n := ft.errorCount(); n != 0 {
		t.Errorf("LeakCheck reported %d error(s) with no leaked goroutines: %v", n, ft.errors)
	}
}

func TestLeakCheckReportsLeakedGoroutine(t *testing.T) {
	ft := &fakeCleanupTB{TB: t}
	testutil.LeakCheck(ft)
	stop := make(chan struct{})
	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		<-stop // deliberately still blocked when cleanup runs
	}()
	<-started
	ft.runCleanups() // polls its full 2s budget, then must report
	close(stop)
	<-done
	if ft.errorCount() == 0 {
		t.Fatal("LeakCheck did not report the leaked goroutine")
	}
	ft.mu.Lock()
	msg := strings.Join(ft.errors, "\n")
	ft.mu.Unlock()
	if !strings.Contains(msg, "cablecheck/internal") {
		t.Errorf("LeakCheck error does not include offending stacks:\n%s", msg)
	}
}

// TestLeakCheckSelfUse: the plain usage pattern at the top of a test must not
// fire for a test that cleans up after itself.
func TestLeakCheckSelfUse(t *testing.T) {
	testutil.LeakCheck(t)
	done := make(chan struct{})
	go close(done)
	<-done
}
