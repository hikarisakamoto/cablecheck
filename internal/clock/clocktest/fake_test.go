package clocktest_test

import (
	"testing"
	"time"

	"cablecheck/internal/clock"
	"cablecheck/internal/clock/clocktest"
	"cablecheck/internal/testutil"
)

// FakeClock must satisfy the canonical clock.Clock contract.
var _ clock.Clock = (*clocktest.FakeClock)(nil)

var start = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func TestFakeClockNowAdvances(t *testing.T) {
	fc := clocktest.New(start)
	if got := fc.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want start %v", got, start)
	}
	fc.Advance(1500 * time.Millisecond)
	want := start.Add(1500 * time.Millisecond)
	if got := fc.Now(); !got.Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", got, want)
	}
	fc.Advance(0)
	if got := fc.Now(); !got.Equal(want) {
		t.Fatalf("Now() after Advance(0) = %v, want %v", got, want)
	}
}

// TestFakeClockAfterWakesWaiter is the core lost-wakeup test: a goroutine
// calls After(d), the test does BlockUntilWaiters(1) + Advance(d) and the
// goroutine must wake. Repeated many times to shake out ordering races.
func TestFakeClockAfterWakesWaiter(t *testing.T) {
	testutil.LeakCheck(t)
	fc := clocktest.New(start)
	const rounds = 50
	for i := 0; i < rounds; i++ {
		done := make(chan struct{})
		go func() {
			<-fc.After(time.Second)
			close(done)
		}()
		fc.BlockUntilWaiters(1)
		fc.Advance(time.Second)
		testutil.WaitFor(t, done, "After waiter to wake")
	}
}

func TestFakeClockAfterDeliversFireTime(t *testing.T) {
	fc := clocktest.New(start)
	ch := fc.After(3 * time.Second)
	fc.Advance(5 * time.Second)
	select {
	case got := <-ch:
		want := start.Add(3 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("After delivered %v, want scheduled fire time %v", got, want)
		}
	default:
		t.Fatal("After channel did not fire after Advance past deadline")
	}
}

func TestFakeClockAfterNotDueDoesNotFire(t *testing.T) {
	fc := clocktest.New(start)
	ch := fc.After(10 * time.Second)
	fc.Advance(9 * time.Second)
	select {
	case <-ch:
		t.Fatal("After fired before its deadline")
	default:
	}
	fc.Advance(time.Second)
	select {
	case <-ch:
	default:
		t.Fatal("After did not fire once its deadline was reached")
	}
}

func TestFakeClockAfterNonPositiveFiresImmediately(t *testing.T) {
	fc := clocktest.New(start)
	select {
	case got := <-fc.After(0):
		if !got.Equal(start) {
			t.Fatalf("After(0) delivered %v, want %v", got, start)
		}
	default:
		t.Fatal("After(0) did not fire immediately")
	}
}

func TestFakeClockBlockUntilWaitersCounts(t *testing.T) {
	testutil.LeakCheck(t)
	fc := clocktest.New(start)
	fc.BlockUntilWaiters(0) // must return immediately

	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-fc.After(time.Second)
			done <- struct{}{}
		}()
	}
	fc.BlockUntilWaiters(2)
	fc.Advance(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(testutil.TestTimeout(t)):
			t.Fatalf("waiter %d never woke", i)
		}
	}
}

// TestFakeClockTickerFiresPerInterval: one tick per interval advanced,
// none for partial intervals, none after Stop.
func TestFakeClockTickerFiresPerInterval(t *testing.T) {
	fc := clocktest.New(start)
	interval := 10 * time.Second
	tk := fc.NewTicker(interval)
	for i := 1; i <= 3; i++ {
		fc.Advance(interval)
		select {
		case got := <-tk.C():
			want := start.Add(time.Duration(i) * interval)
			if !got.Equal(want) {
				t.Fatalf("tick %d delivered %v, want %v", i, got, want)
			}
		default:
			t.Fatalf("tick %d not delivered after advancing one interval", i)
		}
	}
	fc.Advance(interval / 2)
	select {
	case <-tk.C():
		t.Fatal("tick delivered after advancing half an interval")
	default:
	}
	fc.Advance(interval / 2)
	select {
	case <-tk.C():
	default:
		t.Fatal("tick not delivered after completing the interval")
	}
	tk.Stop()
	fc.Advance(3 * interval)
	select {
	case <-tk.C():
		t.Fatal("tick delivered after Stop")
	default:
	}
	tk.Stop() // Stop must be idempotent
}

// TestFakeClockTickerCoalescesWhenUndrained pins time.Ticker-like behavior:
// a multi-interval Advance with no receiver leaves exactly one pending tick
// (buffered channel of one; Advance never blocks).
func TestFakeClockTickerCoalescesWhenUndrained(t *testing.T) {
	fc := clocktest.New(start)
	tk := fc.NewTicker(time.Second)
	defer tk.Stop()
	fc.Advance(3 * time.Second)
	select {
	case got := <-tk.C():
		want := start.Add(time.Second)
		if !got.Equal(want) {
			t.Fatalf("pending tick = %v, want first scheduled tick %v", got, want)
		}
	default:
		t.Fatal("expected one pending tick after multi-interval Advance")
	}
	select {
	case <-tk.C():
		t.Fatal("more than one pending tick; extra ticks must be dropped")
	default:
	}
	// The schedule keeps its cadence: next tick lands on the 4s boundary.
	fc.Advance(time.Second)
	select {
	case got := <-tk.C():
		want := start.Add(4 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("resumed tick = %v, want %v", got, want)
		}
	default:
		t.Fatal("expected tick after advancing to next boundary")
	}
}

func TestFakeClockTickerCountsAsWaiter(t *testing.T) {
	testutil.LeakCheck(t)
	fc := clocktest.New(start)
	done := make(chan struct{})
	go func() {
		tk := fc.NewTicker(time.Second)
		defer tk.Stop()
		<-tk.C()
		close(done)
	}()
	fc.BlockUntilWaiters(1)
	fc.Advance(time.Second)
	testutil.WaitFor(t, done, "ticker tick to wake goroutine")
}

func TestFakeClockNewTickerPanicsOnNonPositive(t *testing.T) {
	fc := clocktest.New(start)
	defer func() {
		if recover() == nil {
			t.Error("NewTicker(0) did not panic")
		}
	}()
	fc.NewTicker(0)
}
