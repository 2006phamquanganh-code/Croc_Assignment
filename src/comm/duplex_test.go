package comm

import (
	"errors"
	"net"
	"testing"
	"time"
)

// Observe the first read to ensure it has applied its deadline before checking
// that a blocked write retains the expired write deadline.
type readObservedConn struct {
	net.Conn
	readStarted chan struct{}
}

func (c *readObservedConn) Read(p []byte) (int, error) {
	select {
	case <-c.readStarted:
	default:
		close(c.readStarted)
	}
	return c.Conn.Read(p)
}

func TestReceivePreservesConcurrentWriteDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	observed := &readObservedConn{Conn: a, readStarted: make(chan struct{})}
	c := New(observed)
	if err := a.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.ReceiveWithDeadline(time.Now().Add(time.Second)); done <- err }()
	<-observed.readStarted
	writeDone := make(chan error, 1)
	go func() { writeDone <- c.Send([]byte("blocked")) }()
	select {
	case err := <-writeDone:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("expected write timeout, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader cleared the write deadline")
	}
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close did not unblock reader")
	}
}

func TestExpiredReadDeadlineDoesNotPreventWrite(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c, peer := New(a), New(b)
	if _, err := c.ReceiveWithDeadline(time.Now().Add(-time.Second)); err == nil {
		t.Fatal("expected read timeout")
	}
	done := make(chan error, 1)
	go func() { done <- c.Send([]byte("still writable")) }()
	data, err := peer.ReceiveWithDeadline(time.Now().Add(time.Second))
	if err != nil || string(data) != "still writable" {
		t.Fatalf("independent write failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
