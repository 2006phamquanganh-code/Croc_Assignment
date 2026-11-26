package chat

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/schollz/croc/v11/src/codephrase"
	"github.com/schollz/croc/v11/src/publicrelay"
	"github.com/schollz/croc/v11/src/tcp"
)

// EventType identifies a lifecycle or delivery event. Acknowledged means the
// remote client accepted the message, not that a person read it.
type EventType uint8

const (
	Connected EventType = iota + 1
	MessageReceived
	Sent
	Acknowledged
	Uncertain
	Closed
	Failed
	MessageExpired
	PeerTyping
	KeyRotated
)

type Event struct {
	Type   EventType
	ID     uint64
	Text   string
	TTL    time.Duration
	Typing bool
	Reason byte
	Err    error
}

type Config struct {
	RelayAddress     string
	RelayPassword    string
	DisableLocal     bool
	OnlyLocal        bool
	MulticastAddress string
}

var (
	ErrNotConnected = errors.New("chat is not connected")
	ErrQueueFull    = errors.New("chat send queue is full")
	ErrText         = errors.New("chat text must be valid UTF-8 and between 1 and 16384 bytes")
	ErrTTL          = errors.New("chat TTL must be between 1s and 24h")
	ErrDisconnected = errors.New("chat connection ended without an authenticated close")
	ErrOverloaded   = errors.New("chat event or control queue is full")
	ErrTimeout      = errors.New("chat peer did not respond in time")
)

type timings struct{ wait, auth, ready, write, heartbeat, pong, ack, lifetime, close time.Duration }

func defaults() timings {
	return timings{5 * time.Minute, 30 * time.Second, 10 * time.Second, 10 * time.Second, 30 * time.Second, 90 * time.Second, 30 * time.Second, 2 * time.Hour, 2 * time.Second}
}

// StartHost admits a fresh, single-use invitation before returning. Handshake
// proceeds asynchronously; callers should consume Events immediately.
func StartHost(ctx context.Context, cfg Config) (*Session, error) {
	code, err := codephrase.GenerateChat()
	if err != nil {
		return nil, errors.New("could not generate chat invitation")
	}
	return connect(ctx, cfg, code, true)
}

// Join authenticates asynchronously after relay admission.
func Join(ctx context.Context, cfg Config, code string) (*Session, error) {
	return connect(ctx, cfg, strings.TrimSpace(code), false)
}

func connect(ctx context.Context, cfg Config, code string, host bool) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	components, err := codephrase.ParseChat(code)
	if err != nil {
		return nil, err
	}

	useLocal := shouldUseLocal(cfg)
	if !host && useLocal {
		window := localDiscoveryTimeout
		if cfg.OnlyLocal {
			window = onlyLocalDiscoveryWindow
		}
		if localConn, localAddr, localErr := tryJoinLocalChat(ctx, components.RoomName, cfg.RelayPassword, cfg.MulticastAddress, window); localErr == nil {
			s := newSession(ctx, localConn, components, false, code, localAddr, defaults())
			s.setTransport(true, localAddr)
			return s, nil
		} else if cfg.OnlyLocal {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, localErr
		}
	}

	var localHost *localHostRelay
	if host && useLocal {
		if lh, localErr := startLocalHostRelay(ctx, components.RoomName, cfg.RelayPassword, cfg.MulticastAddress); localErr == nil {
			localHost = lh
		} else if cfg.OnlyLocal {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, localErr
		}
	}

	if cfg.OnlyLocal && host && localHost != nil {
		s := newSessionWithLocal(ctx, nil, localHost, components, true, code, localHost.addr, defaults())
		return s, nil
	}

	address := cfg.RelayAddress
	if address == "" {
		relays := publicrelay.Relays()
		index, e := codephrase.RelayIndex(code, len(relays))
		if e != nil {
			if localHost != nil {
				localHost.conn.Close()
				localHost.stopRelay()
			}
			return nil, e
		}
		address = relays[index]
	}
	admission, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, _, _, _, err := tcp.ConnectToTCPServerControlContext(admission, address, cfg.RelayPassword, components.RoomName, 30*time.Second)
	if err != nil {
		if localHost != nil && host {
			// External relay unreachable, but local LAN relay is ready.
			return newSessionWithLocal(ctx, nil, localHost, components, true, code, localHost.addr, defaults()), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if admission.Err() != nil {
			return nil, admission.Err()
		}
		return nil, errors.New("could not join chat relay; check relay settings and availability")
	}
	if localHost != nil {
		return newSessionWithLocal(ctx, c, localHost, components, host, code, address, defaults()), nil
	}
	return newSession(ctx, c, components, host, code, address, defaults()), nil
}

// Code is the host's invitation. Callers must not log it or persist it.
func (s *Session) Code() string         { s.mu.Lock(); defer s.mu.Unlock(); return s.code }
func (s *Session) RelayAddress() string { return s.relay }
func (s *Session) IsLocal() bool        { s.mu.Lock(); defer s.mu.Unlock(); return s.isLocal }
func (s *Session) TransportStats() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return formatTransportStats(s.isLocal, s.transportAddr)
}
func (s *Session) Events() <-chan Event { return s.events }
func (s *Session) Wait() error          { <-s.done; s.mu.Lock(); defer s.mu.Unlock(); return s.err }

// Close requests an authenticated shutdown, with an absolute bound even if the
// writer is stuck. Repeated calls are safe; Wait reports the terminal outcome.
func (s *Session) Close() { s.requestClose(closeNormal) }

// SendText accepts a copy into a bounded queue. Success does not mean delivery.
func (s *Session) SendText(text string) (uint64, error) {
	if err := validateRecord(record{kind: typeText, seq: 1, payload: []byte(text)}); err != nil {
		return 0, ErrText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.closing {
		return 0, ErrNotConnected
	}
	if s.outstanding >= 64 {
		return 0, ErrQueueFull
	}
	s.id++
	out := outgoing{kind: typeText, payload: []byte(text), id: s.id}
	select {
	case s.texts <- out:
		s.outstanding++
		if s.accepted == nil {
			s.accepted = make(map[uint64]bool)
		}
		s.accepted[s.id] = false
		return s.id, nil
	default:
		return 0, ErrQueueFull
	}
}

// SendTTLText sends a self-destructing UTF-8 message. Both sender and receiver
// retain the message bytes only until ttl expires, then overwrite the RAM
// buffer with zero bytes and emit MessageExpired.
func (s *Session) SendTTLText(text string, ttl time.Duration) (uint64, error) {
	secs := int64(ttl / time.Second)
	if ttl < time.Second || secs <= 0 || uint64(secs) > uint64(maxTTLSeconds) {
		return 0, ErrTTL
	}
	payload := make([]byte, 4+len(text))
	binary.BigEndian.PutUint32(payload[:4], uint32(secs))
	copy(payload[4:], text)
	if err := validateRecord(record{kind: typeTTLText, seq: 1, payload: payload}); err != nil {
		clear(payload)
		return 0, ErrText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.closing {
		clear(payload)
		return 0, ErrNotConnected
	}
	if s.outstanding >= 64 {
		clear(payload)
		return 0, ErrQueueFull
	}
	s.id++
	id := s.id
	out := outgoing{kind: typeTTLText, payload: payload, id: id}
	select {
	case s.texts <- out:
		s.outstanding++
		if s.accepted == nil {
			s.accepted = make(map[uint64]bool)
		}
		s.accepted[id] = false
		s.trackEphemeralLocked(true, id, []byte(text), time.Duration(secs)*time.Second)
		return id, nil
	default:
		clear(payload)
		return 0, ErrQueueFull
	}
}

// SendTyping notifies the remote peer whether the local user is currently
// typing. It is best-effort and drops silently if the control queue is busy.
func (s *Session) SendTyping(typing bool) error {
	s.mu.Lock()
	active := s.active && !s.closing
	s.mu.Unlock()
	if !active {
		return ErrNotConnected
	}
	var b byte
	if typing {
		b = 1
	}
	s.control(outgoing{kind: typeTyping, payload: []byte{b}})
	return nil
}
