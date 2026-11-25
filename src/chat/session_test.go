package chat

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	log "github.com/schollz/croc/v11/src/logger"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/schollz/croc/v11/src/codephrase"
	"github.com/schollz/croc/v11/src/comm"
)

func sessionPair(t *testing.T) (*Session, *Session) {
	t.Helper()
	a, b := net.Pipe()
	c, err := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
	if err != nil {
		t.Fatal(err)
	}
	tm := defaults()
	tm.close = 100 * time.Millisecond
	tm.heartbeat = 20 * time.Millisecond
	tm.pong = time.Second
	x := newSession(t.Context(), comm.New(a), c, true, "", "", tm)
	y := newSession(t.Context(), comm.New(b), c, false, "", "", tm)
	t.Cleanup(func() { x.Close(); y.Close(); x.Wait(); y.Wait() })
	return x, y
}
func waitEvent(t *testing.T, s *Session, kind EventType) Event {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("events ended before %v, wait error %v", kind, s.Wait())
			}
			if e.Type == Failed {
				t.Fatalf("session failed: %v", e.Err)
			}
			if e.Type == kind {
				return e
			}
		case <-timeout.C:
			t.Fatalf("timeout waiting for event %v", kind)
		}
	}
}
func TestSessionBidirectionalDelivery(t *testing.T) {
	a, b := sessionPair(t)
	waitEvent(t, a, Connected)
	waitEvent(t, b, Connected)
	ai, err := a.SendText("Chào B 👋")
	if err != nil {
		t.Fatal(err)
	}
	bi, err := b.SendText("Xin chào A")
	if err != nil {
		t.Fatal(err)
	}
	// Consume both directions without assuming whether Sent precedes an early ACK.
	for _, test := range []struct {
		s    *Session
		text string
		id   uint64
	}{{a, "Xin chào A", ai}, {b, "Chào B 👋", bi}} {
		received, ack := false, false
		deadline := time.After(3 * time.Second)
		for !received || !ack {
			select {
			case e := <-test.s.Events():
				if e.Type == MessageReceived {
					received = e.Text == test.text
				}
				if e.Type == Acknowledged {
					ack = e.ID == test.id
				}
				if e.Type == Failed {
					t.Fatal(e.Err)
				}
			case <-deadline:
				t.Fatal("missing message or ACK")
			}
		}
	}
	a.Close()
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b.Close() // Closing a completed session is a no-op.
}
func TestSessionHeartbeatAndSimultaneousClose(t *testing.T) {
	a, b := sessionPair(t)
	waitEvent(t, a, Connected)
	waitEvent(t, b, Connected)
	time.Sleep(120 * time.Millisecond)
	a.mu.Lock()
	activeA := a.active
	a.mu.Unlock()
	b.mu.Lock()
	activeB := b.active
	b.mu.Unlock()
	if !activeA || !activeB {
		t.Fatal("heartbeat ended active session")
	}
	go a.Close()
	b.Close()
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
}
func TestSessionCancelWaitingForPeer(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c, err := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := newSession(ctx, comm.New(a), c, true, "", "", defaults())
	cancel()
	if err := s.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
func TestSessionCloseWaitingForPeer(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c, err := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(t.Context(), comm.New(a), c, true, "", "", defaults())
	if _, err := s.SendText("early"); !errors.Is(err, ErrNotConnected) {
		t.Fatal(err)
	}
	s.Close()
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
}
func TestSessionQueueBoundAndValidation(t *testing.T) {
	s := &Session{active: true, texts: make(chan outgoing, 64)}
	for i := 0; i < 64; i++ {
		if _, err := s.SendText("queued"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SendText("overflow"); !errors.Is(err, ErrQueueFull) {
		t.Fatal("queue exceeded limit")
	}
	for _, text := range []string{"", string([]byte{255})} {
		if _, err := s.SendText(text); !errors.Is(err, ErrText) {
			t.Fatal("accepted invalid text")
		}
	}
}

// rawPeer starts one full session with an authenticated peer controlled by the
// test. The test can stop reading, withhold ACK/PONG, or inject invalid controls.
func rawPeer(t *testing.T, adjust func(*timings)) (*Session, *comm.Comm, *recordCipher, *recordCipher) {
	t.Helper()
	return rawPeerWrapped(t, adjust, nil)
}
func rawPeerWrapped(t *testing.T, adjust func(*timings), wrap func(net.Conn) net.Conn) (*Session, *comm.Comm, *recordCipher, *recordCipher) {
	t.Helper()
	a, b := net.Pipe()
	if wrap != nil {
		a = wrap(a)
	}
	c, err := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
	if err != nil {
		t.Fatal(err)
	}
	tm := defaults()
	tm.close = 80 * time.Millisecond
	if adjust != nil {
		adjust(&tm)
	}
	s := newSession(t.Context(), comm.New(a), c, true, "", "", tm)
	peer := comm.New(b)
	t.Cleanup(func() { peer.Close(); s.Close(); s.Wait() })
	send, recv, err := handshake(peer, c, false, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body, err := peer.ReceiveWithDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recv.open(body); err != nil {
		t.Fatal(err)
	}
	body, err = send.seal(typeReady, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(body); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, s, Connected)
	return s, peer, send, recv
}
func TestCloseInterruptsBlockedWriter(t *testing.T) {
	s, _, _, _ := rawPeer(t, nil)
	if _, err := s.SendText("blocked write"); err != nil {
		t.Fatal(err)
	}
	// The peer reads nothing: either TEXT or CLOSE will block in net.Pipe.
	started := time.Now()
	s.Close()
	s.Wait()
	if time.Since(started) > time.Second {
		t.Fatal("close waited for the longer write timeout")
	}
}
func TestSessionACKTimeoutAndLateACK(t *testing.T) {
	s, peer, send, recv := rawPeer(t, func(tm *timings) { tm.ack = 20 * time.Millisecond })
	id, err := s.SendText("ack me")
	if err != nil {
		t.Fatal(err)
	}
	body, err := peer.ReceiveWithDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	text, err := recv.open(body)
	if err != nil {
		t.Fatal(err)
	}
	if e := waitEvent(t, s, Uncertain); e.ID != id {
		t.Fatal("wrong uncertain message")
	}
	ref := make([]byte, 8)
	binary.BigEndian.PutUint64(ref, text.seq)
	body, err = send.seal(typeAck, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(body); err != nil {
		t.Fatal(err)
	}
	if e := waitEvent(t, s, Acknowledged); e.ID != id {
		t.Fatal("wrong ACK")
	}
	// A second ACK is authenticated but violates protocol semantics.
	body, err = send.seal(typeAck, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(body); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); !errors.Is(err, errProtocol) {
		t.Fatalf("duplicate ACK: %v", err)
	}
}
func TestSessionMissingPONG(t *testing.T) {
	s, peer, _, recv := rawPeer(t, func(tm *timings) { tm.heartbeat = 10 * time.Millisecond; tm.pong = 60 * time.Millisecond })
	body, err := peer.ReceiveWithDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ping, err := recv.open(body)
	if err != nil || ping.kind != typePing {
		t.Fatalf("missing PING: %v", err)
	}
	if err := s.Wait(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("missing PONG: %v", err)
	}
}

func TestSessionLifetimeClosesAuthenticated(t *testing.T) {
	s, peer, _, recv := rawPeer(t, func(tm *timings) { tm.lifetime = 20 * time.Millisecond })
	body, err := peer.ReceiveWithDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	r, err := recv.open(body)
	if err != nil || r.kind != typeClose || r.payload[0] != closeExpired {
		t.Fatalf("expiry CLOSE: %+v %v", r, err)
	}
	// The client must also stop when the peer never reciprocates CLOSE.
	started := time.Now()
	s.Wait()
	if time.Since(started) > time.Second {
		t.Fatal("close deadline was not enforced")
	}
}
func TestSessionMalformedPONGFails(t *testing.T) {
	s, peer, send, _ := rawPeer(t, nil)
	body, err := send.seal(typePong, make([]byte, 8))
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(body); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); !errors.Is(err, errProtocol) {
		t.Fatalf("unsolicited PONG: %v", err)
	}
}
func TestSessionSlowConsumerIsBounded(t *testing.T) {
	s, peer, send, _ := rawPeer(t, nil)
	// Each payload is valid, but the caller deliberately does not consume Events.
	for i := 0; i < 140; i++ {
		body, err := send.seal(typeText, []byte("flood"))
		if err != nil {
			t.Fatal(err)
		}
		if err := peer.Connection().SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := peer.Send(body); err != nil {
			break
		}
	}
	started := time.Now()
	s.Wait()
	if time.Since(started) > time.Second {
		t.Fatal("overload did not stop session")
	}
	if len(s.events) > cap(s.events) {
		t.Fatal("unbounded event queue")
	}
}

func TestCloseTimeoutWhenPeerReadsButDoesNotReply(t *testing.T) {
	s, peer, _, recv := rawPeer(t, nil)
	s.Close()
	body, err := peer.ReceiveWithDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	r, err := recv.open(body)
	if err != nil || r.kind != typeClose {
		t.Fatalf("CLOSE: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDisconnected) {
			t.Fatalf("unconfirmed close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close hung after successful CLOSE write")
	}
}

type gatedWriteConn struct {
	net.Conn
	enabled atomic.Bool
	writes  atomic.Int32
	release chan struct{}
}

func (c *gatedWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.enabled.Load() && c.writes.Add(1) == 2 {
		<-c.release
	}
	return n, err
}
func TestACKBeforeWriteReturns(t *testing.T) {
	a, b := net.Pipe()
	gated := &gatedWriteConn{Conn: a, release: make(chan struct{})}
	components, err := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
	if err != nil {
		t.Fatal(err)
	}
	tm := defaults()
	tm.close = 80 * time.Millisecond
	s := newSession(t.Context(), comm.New(gated), components, true, "", "", tm)
	peer := comm.New(b)
	var released sync.Once
	release := func() { released.Do(func() { close(gated.release) }) }
	t.Cleanup(func() { release(); peer.Close(); s.Close(); s.Wait() })
	send, recv, err := handshake(peer, components, false, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body, err := peer.ReceiveWithDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recv.open(body); err != nil {
		t.Fatal(err)
	}
	body, err = send.seal(typeReady, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(body); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, s, Connected)
	gated.writes.Store(0)
	gated.enabled.Store(true)
	id, err := s.SendText("fast ACK")
	if err != nil {
		t.Fatal(err)
	}
	body, err = peer.ReceiveWithDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	text, err := recv.open(body)
	if err != nil {
		t.Fatal(err)
	}
	ref := make([]byte, 8)
	binary.BigEndian.PutUint64(ref, text.seq)
	body, err = send.seal(typeAck, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(body); err != nil {
		t.Fatal(err)
	}
	if e := waitEvent(t, s, Acknowledged); e.ID != id {
		t.Fatal("wrong message ACK")
	}
	release()
	s.Close()
	for e := range s.Events() {
		if e.Type == Sent && e.ID == id {
			t.Fatal("Sent overwrote an earlier ACK")
		}
	}
}

func TestChatDebugLogsExcludeSecretsAndContent(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "chat-log-")
	if err != nil {
		t.Fatal(err)
	}
	previous := log.GetLevel()
	log.SetOutput(f)
	log.SetLevel("debug")
	defer func() { log.SetOutput(os.Stdout); log.SetLevel(previous); f.Close() }()
	a, b := sessionPair(t)
	waitEvent(t, a, Connected)
	waitEvent(t, b, Connected)
	const text = "private-message-should-not-be-logged-7c4a"
	if _, err := a.SendText(text); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, b, MessageReceived)
	waitEvent(t, a, Acknowledged)
	a.Close()
	a.Wait()
	b.Wait()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	c, _ := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
	for _, secret := range []string{text, c.RoomName, c.PAKEPassphrase, "acid-acorn-acre-acts-ahead-alien"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("debug logs leaked chat secret or content")
		}
	}
}

type deadlineFailureConn struct {
	net.Conn
	fail atomic.Bool
}

func (c *deadlineFailureConn) SetWriteDeadline(deadline time.Time) error {
	if c.fail.Load() {
		return errors.New("injected write deadline failure")
	}
	return c.Conn.SetWriteDeadline(deadline)
}
func TestAcceptedTextIsUncertainWhenSetupWriteFails(t *testing.T) {
	var conn *deadlineFailureConn
	s, _, _, _ := rawPeerWrapped(t, nil, func(c net.Conn) net.Conn { conn = &deadlineFailureConn{Conn: c}; return conn })
	conn.fail.Store(true)
	id, err := s.SendText("accepted before write setup failure")
	if err != nil {
		t.Fatal(err)
	}
	uncertain := 0
	for event := range s.Events() {
		if event.Type == Uncertain && event.ID == id {
			uncertain++
		}
		if event.Type == Acknowledged {
			t.Fatal("failed write was acknowledged")
		}
	}
	if err := s.Wait(); err == nil {
		t.Fatal("missing write setup error")
	}
	if uncertain != 1 {
		t.Fatalf("accepted message had %d Uncertain events", uncertain)
	}
}
func TestParentDeadlineIsPreserved(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	components, err := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	s := newSession(ctx, comm.New(a), components, true, "", "", defaults())
	if err := s.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent deadline error changed: %v", err)
	}
}

func TestJoinPreservesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	session, err := Join(ctx, Config{RelayAddress: "127.0.0.1:1"}, "acid-acorn-acre-acts-ahead-alien")
	if session != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission: %v", err)
	}
}
