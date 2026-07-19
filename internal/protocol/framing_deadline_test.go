package protocol

import (
	"net"
	"sync"
	"testing"
	"time"
)

type interleavedDeadlineConn struct {
	net.Conn

	mu           sync.Mutex
	deadlines    []time.Time
	firstEntered chan struct{}
	releaseFirst chan struct{}
}

func (c *interleavedDeadlineConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	first := len(c.deadlines) == 0
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	if first {
		close(c.firstEntered)
		<-c.releaseFirst
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *interleavedDeadlineConn) lastDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadlines[len(c.deadlines)-1]
}

func TestSetIdleTimeoutCannotBeClobberedByStalePerFrameDeadline(t *testing.T) {
	left, right := net.Pipe()
	recorded := &interleavedDeadlineConn{
		Conn: left, firstEntered: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
	conn := NewConn(recorded)
	t.Cleanup(func() {
		conn.Close()
		right.Close()
	})

	readDone := make(chan error, 1)
	go func() {
		_, err := conn.ReadEnvelope()
		readDone <- err
	}()
	<-recorded.firstEntered

	if conn.deadlineMu.TryLock() {
		conn.deadlineMu.Unlock()
		t.Errorf("per-frame SetReadDeadline did not hold deadlineMu")
	}
	setStarted := make(chan struct{})
	setDone := make(chan struct{})
	go func() {
		close(setStarted)
		conn.SetIdleTimeout(4 * time.Minute)
		close(setDone)
	}()
	<-setStarted
	close(recorded.releaseFirst)
	<-setDone

	if got := time.Until(recorded.lastDeadline()); got < 3*time.Minute {
		t.Errorf("effective deadline is only %v away, want widened four-minute deadline", got)
	}
	conn.Close()
	<-readDone
}
