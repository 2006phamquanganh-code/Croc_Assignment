package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/schollz/croc/v11/src/codephrase"
	"github.com/schollz/croc/v11/src/comm"
)

type outgoing struct {
	kind    recordType
	payload []byte
	id      uint64
}
type pendingText struct {
	id        uint64
	sent      time.Time
	uncertain bool
}

// Session owns a single authenticated conversation and all of its goroutines.
type Session struct {
	code, relay               string
	conn                      *comm.Comm
	ctx                       context.Context
	cancel                    context.CancelFunc
	stopContext               func() bool
	times                     timings
	events                    chan Event
	texts                     chan outgoing
	controls                  chan outgoing
	closeRequest              chan struct{}
	closeExpiredCh            chan struct{}
	done                      chan struct{}
	mu                        sync.Mutex
	active, closing, finished bool
	closeReason               byte
	closeTimer                *time.Timer
	err                       error
	id                        uint64
	outstanding               int
	accepted                  map[uint64]bool // IDs accepted by SendText; value records an Uncertain event.
	pending                   map[uint64]pendingText
	ping                      []byte
	pingSent                  time.Time
}

func newSession(ctx context.Context, c *comm.Comm, components codephrase.ChatComponents, host bool, code, relay string, tm timings) *Session {
	ctx, cancel := context.WithCancel(ctx)
	s := &Session{code: code, relay: relay, conn: c, ctx: ctx, cancel: cancel, times: tm, events: make(chan Event, 64), texts: make(chan outgoing, 64), controls: make(chan outgoing, 128), closeRequest: make(chan struct{}), closeExpiredCh: make(chan struct{}), done: make(chan struct{}), pending: make(map[uint64]pendingText), accepted: make(map[uint64]bool)}
	s.stopContext = context.AfterFunc(ctx, c.Close)
	go s.run(components, host)
	return s
}

// emitLocked reserves one slot for the terminal event. Caller holds mu.
func (s *Session) emitLocked(e Event) bool {
	if len(s.events) >= cap(s.events)-1 {
		return false
	}
	s.events <- e
	return true
}
func (s *Session) emit(e Event) bool { s.mu.Lock(); defer s.mu.Unlock(); return s.emitLocked(e) }

func (s *Session) requestClose(reason byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestCloseUnlocked(reason)
}

func (s *Session) run(components codephrase.ChatComponents, host bool) {
	err := s.converse(components, host)
	contextErr := s.ctx.Err()
	s.conn.Close()
	s.cancel()
	s.stopContext()
	s.mu.Lock()
	if s.closeTimer != nil {
		s.closeTimer.Stop()
	}
	s.active = false
	s.finished = true
	// A local close during authentication is cancellation, not a peer failure.
	if s.closing && errors.Is(err, errBeforeReady) {
		err = nil
	}
	if contextErr != nil && err != nil && !s.closing {
		err = contextErr
	}
	s.err = err
	for id, uncertain := range s.accepted {
		if !uncertain {
			s.emitLocked(Event{Type: Uncertain, ID: id})
		}
	}
	clear(s.accepted)
	s.accepted = nil
	clear(s.pending)

	for {
		select {
		case out := <-s.texts:
			clear(out.payload)
		default:
			goto drained
		}
	}
drained:
	clear(s.ping)
	s.ping = nil
	s.pending = nil
	s.outstanding = 0
	s.code = ""
	terminal := Event{Type: Closed, Reason: s.closeReason, Err: err}
	if err != nil {
		terminal.Type = Failed
	}
	s.events <- terminal
	close(s.events)
	s.mu.Unlock()
	close(s.done)
}

var errBeforeReady = errors.New("chat ended before READY")

func (s *Session) converse(components codephrase.ChatComponents, host bool) error {
	// Close during the initial wait must interrupt authentication promptly.
	authDone := make(chan struct{})
	authWatcherDone := make(chan struct{})
	go func() {
		defer close(authWatcherDone)
		select {
		case <-s.closeRequest:
			s.conn.Close()
		case <-authDone:
		}
	}()
	send, recv, err := handshake(s.conn, components, host, s.times.wait, s.times.auth)
	close(authDone)
	<-authWatcherDone
	if err != nil {
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return errBeforeReady
		}
		return errHandshake
	}
	// A close racing authentication must never activate a session.
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return errBeforeReady
	}
	readyDeadline := time.Now().Add(s.times.ready)
	// Keep one in-flight decoded record rather than a second 64-message queue.
	// At most 63 buffered events plus this record can retain received text.
	incoming := make(chan readResult)
	writerErrors := make(chan error, 1)
	sentReady := make(chan struct{})
	sentClose := make(chan struct{}, 1)
	workersCtx, cancel := context.WithCancel(s.ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.readLoop(workersCtx, recv, readyDeadline, incoming) }()
	go func() {
		defer wg.Done()
		s.writeLoop(workersCtx, send, readyDeadline, sentReady, sentClose, writerErrors)
	}()
	defer func() { cancel(); s.conn.Close(); wg.Wait() }()
	readyTimer := time.NewTimer(time.Until(readyDeadline))
	defer readyTimer.Stop()
	localReady, remoteReady := false, false
	readyIncoming := incoming
	for !localReady || !remoteReady {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-s.closeRequest:
			return errBeforeReady
		case <-sentReady:
			localReady = true
			sentReady = nil
		case result := <-readyIncoming:
			if result.err != nil {
				return result.err
			}
			r := result.record
			if remoteReady || r.kind != typeReady {
				return errProtocol
			}
			remoteReady = true
			readyIncoming = nil
		case err := <-writerErrors:
			return err
		case <-readyTimer.C:
			return ErrTimeout
		}
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return errBeforeReady
	}
	s.active = true
	ok := s.emitLocked(Event{Type: Connected})
	s.mu.Unlock()
	if !ok {
		return ErrOverloaded
	}
	// READY from both peers has been processed; subsequent records may already
	// be buffered in incoming and are handled in order by the same coordinator.
	tickInterval := min(time.Second, s.times.heartbeat, s.times.ack)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	lifetime := time.NewTimer(s.times.lifetime)
	defer lifetime.Stop()
	nextPing := time.Now().Add(s.times.heartbeat)
	closeWritten, closeReceived := false, false
	closeCh := s.closeRequest
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-s.closeExpiredCh:
			if closeReceived && closeWritten {
				return nil
			}
			return ErrDisconnected
		case <-closeCh: // writer observes closing state independently
			closeCh = nil
		case <-sentClose:
			closeWritten = true
			if closeReceived {
				return nil
			}
		case <-lifetime.C:
			s.requestClose(closeExpired)
		case err := <-writerErrors:
			return err
		case result := <-incoming:
			if result.err != nil {
				return result.err
			}
			r := result.record
			if r.kind == typeClose {
				closeReceived = true
				s.requestClose(closeNormal)
				if closeWritten {
					return nil
				}
				continue
			}
			if closeReceived {
				return errProtocol
			}
			if err := s.handleRecord(r); err != nil {
				if errors.Is(err, ErrOverloaded) {
					s.requestClose(closeOverloaded)
				} else {
					return err
				}
			}
		case now := <-ticker.C:
			s.mu.Lock()
			closing := s.closing
			expired := len(s.ping) > 0 && !s.pingSent.IsZero() && now.Sub(s.pingSent) >= s.times.pong
			for seq, p := range s.pending {
				if !p.uncertain && !p.sent.IsZero() && now.Sub(p.sent) >= s.times.ack {
					p.uncertain = true
					s.pending[seq] = p
					if !s.emitLocked(Event{Type: Uncertain, ID: p.id}) {
						closing = true
					} else {
						s.accepted[p.id] = true
					}
				}
			}
			s.mu.Unlock()
			if expired {
				return ErrTimeout
			}
			if closing {
				s.mu.Lock()
				requested := s.closing
				s.mu.Unlock()
				if !requested {
					s.requestClose(closeOverloaded)
				}
				continue
			}
			if !now.Before(nextPing) {
				s.mu.Lock()
				hasPing := len(s.ping) > 0
				s.mu.Unlock()
				if !hasPing {
					token := make([]byte, 8)
					if _, err := rand.Read(token); err != nil {
						return err
					}
					s.mu.Lock()
					s.ping = append([]byte(nil), token...)
					s.pingSent = time.Time{}
					s.mu.Unlock()
					if !s.control(outgoing{kind: typePing, payload: token}) {
						s.requestClose(closeOverloaded)
					}
				}
				nextPing = now.Add(s.times.heartbeat)
			}
		}
	}
}

func (s *Session) control(out outgoing) bool {
	select {
	case s.controls <- out:
		return true
	default:
		return false
	}
}
func (s *Session) handleRecord(r record) error {
	switch r.kind {
	case typeText:
		if !s.emit(Event{Type: MessageReceived, Text: string(r.payload)}) {
			return ErrOverloaded
		}
		ref := make([]byte, 8)
		binary.BigEndian.PutUint64(ref, r.seq)
		if !s.control(outgoing{kind: typeAck, payload: ref}) {
			return ErrOverloaded
		}
	case typeAck:
		s.mu.Lock()
		defer s.mu.Unlock()
		seq := binary.BigEndian.Uint64(r.payload)
		p, ok := s.pending[seq]
		if !ok {
			return errProtocol
		}
		delete(s.pending, seq)
		delete(s.accepted, p.id)
		s.outstanding--
		if !s.emitLocked(Event{Type: Acknowledged, ID: p.id}) {
			return ErrOverloaded
		}
	case typePing:
		if !s.control(outgoing{kind: typePong, payload: r.payload}) {
			return ErrOverloaded
		}
	case typePong:
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.ping) == 0 || !bytes.Equal(s.ping, r.payload) {
			return errProtocol
		}
		s.ping = nil
		s.pingSent = time.Time{}
	default:
		return errProtocol
	}
	return nil
}

type readResult struct {
	record record
	err    error
}

func (s *Session) readLoop(ctx context.Context, c *recordCipher, readyDeadline time.Time, out chan<- readResult) {
	deliver := func(result readResult) bool {
		select {
		case out <- result:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		deadline := time.Now().Add(2*s.times.pong + s.times.heartbeat)
		if c.next == 0 {
			deadline = readyDeadline
		}
		body, err := s.conn.ReceiveWithDeadlineLimit(deadline, maxFrameSize)
		if err != nil {
			select {
			case out <- readResult{err: ErrDisconnected}:
			case <-ctx.Done():
			}
			return
		}
		if c.next == 0 && bytes.Equal(body, []byte{1}) {
			continue
		}
		r, err := c.open(body)
		if err != nil {
			select {
			case out <- readResult{err: err}:
			case <-ctx.Done():
			}
			return
		}
		if !deliver(readResult{record: r}) || r.kind == typeClose {
			return
		}
	}
}

func (s *Session) writeLoop(ctx context.Context, c *recordCipher, readyDeadline time.Time, ready chan<- struct{}, closed chan<- struct{}, errs chan<- error) {
	write := func(out outgoing, deadline time.Time) error {
		seq := c.next
		body, err := c.seal(out.kind, out.payload)
		if err != nil {
			return err
		}
		if err = s.conn.Connection().SetWriteDeadline(deadline); err != nil {
			return err
		}
		if out.kind == typeText {
			s.mu.Lock()
			s.pending[seq] = pendingText{id: out.id}
			s.mu.Unlock()
		}
		err = s.conn.Send(body)
		if err != nil {
			return ErrDisconnected
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if out.kind == typeText {
			if p, ok := s.pending[seq]; ok {
				p.sent = time.Now()
				s.pending[seq] = p
				if !s.emitLocked(Event{Type: Sent, ID: out.id}) {
					s.requestCloseUnlocked(closeOverloaded)
				}
			}
		}
		if out.kind == typePing && bytes.Equal(s.ping, out.payload) {
			s.pingSent = time.Now()
		}
		return nil
	}
	if err := write(outgoing{kind: typeReady}, readyDeadline); err != nil {
		errs <- err
		return
	}
	close(ready)
	for {
		s.mu.Lock()
		closing, reason := s.closing, s.closeReason
		s.mu.Unlock()
		if c.next == maxRecords-1 {
			s.requestClose(closeKeyLimit)
			closing = true
			reason = closeKeyLimit
		}
		if closing {
			if err := write(outgoing{kind: typeClose, payload: []byte{reason}}, time.Now().Add(s.times.close)); err != nil {
				errs <- err
				return
			}
			closed <- struct{}{}
			return
		}
		var out outgoing
		// Control frames have priority over user text; only this worker seals records.
		select {
		case out = <-s.controls:
		default:
			select {
			case <-ctx.Done():
				return
			case <-s.closeRequest:
				continue
			case out = <-s.controls:
			case out = <-s.texts:
			}
		}
		s.mu.Lock()
		closing = s.closing
		s.mu.Unlock()
		if closing {

			clear(out.payload)
			continue
		}
		err := write(out, time.Now().Add(s.times.write))
		clear(out.payload)
		if err != nil {
			errs <- err
			return
		}
	}
}

// requestCloseUnlocked requires mu and broadcasts to all session workers.
func (s *Session) requestCloseUnlocked(reason byte) {
	if s.closing || s.finished {
		return
	}
	s.closing = true
	s.active = false
	s.closeReason = reason
	s.closeTimer = time.AfterFunc(s.times.close, func() { s.conn.Close(); close(s.closeExpiredCh) })
	close(s.closeRequest)
}
