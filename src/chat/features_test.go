package chat

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

func TestChatTTLMessageExpiresAndWipesRAM(t *testing.T) {
	relay := chatTestRelay(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host, err := StartHost(ctx, Config{RelayAddress: relay, RelayPassword: "relay-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { host.Close(); _ = host.Wait() }()

	join, err := Join(ctx, Config{RelayAddress: relay, RelayPassword: "relay-password"}, host.Code())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { join.Close(); _ = join.Wait() }()

	for _, pair := range []struct {
		name string
		s    *Session
	}{{"host", host}, {"join", join}} {
		select {
		case ev := <-pair.s.Events():
			if ev.Type != Connected {
				t.Fatalf("%s expected Connected, got %+v", pair.name, ev)
			}
		case <-ctx.Done():
			t.Fatalf("%s timed out waiting for Connected", pair.name)
		}
	}

	const secret = "one-time-secret-999"
	id, err := host.SendTTLText(secret, time.Second)
	if err != nil {
		t.Fatalf("SendTTLText failed: %v", err)
	}

	// Capture underlying RAM buffer reference on host to verify zero-overwrite.
	host.mu.Lock()
	hostEntry := host.ephemeral[ephemeralKey{outgoing: true, id: id}]
	if hostEntry == nil || string(hostEntry.buf) != secret {
		host.mu.Unlock()
		t.Fatalf("expected host ephemeral buffer to contain secret")
	}
	hostBufRef := hostEntry.buf
	host.mu.Unlock()

	// Verify joiner receives the TTL message and captures its RAM buffer.
	var recvID uint64
	var joinBufRef []byte
	for recvID == 0 {
		select {
		case ev := <-join.Events():
			if ev.Type == MessageReceived {
				if ev.Text != secret || ev.TTL != time.Second {
					t.Fatalf("unexpected TTL receive event: %+v", ev)
				}
				recvID = ev.ID
				join.mu.Lock()
				if entry := join.ephemeral[ephemeralKey{outgoing: false, id: recvID}]; entry != nil {
					joinBufRef = entry.buf
				}
				join.mu.Unlock()
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for TTL message")
		}
	}

	// Wait for host ACK and MessageExpired.
	gotAck, gotHostExpired := false, false
	for !gotAck || !gotHostExpired {
		select {
		case ev := <-host.Events():
			if ev.Type == Acknowledged && ev.ID == id {
				gotAck = true
			}
			if ev.Type == MessageExpired && ev.ID == id {
				gotHostExpired = true
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for host ack=%v expired=%v", gotAck, gotHostExpired)
		}
	}

	// Wait for joiner MessageExpired.
	gotJoinExpired := false
	for !gotJoinExpired {
		select {
		case ev := <-join.Events():
			if ev.Type == MessageExpired && ev.ID == recvID {
				gotJoinExpired = true
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for joiner MessageExpired")
		}
	}

	zeros := make([]byte, len(secret))
	if !bytes.Equal(hostBufRef, zeros) {
		t.Fatalf("host RAM buffer was not zeroed: %x", hostBufRef)
	}
	if len(joinBufRef) == 0 || !bytes.Equal(joinBufRef, zeros) {
		t.Fatalf("joiner RAM buffer was not zeroed: %x", joinBufRef)
	}
}

func TestChatTypingIndicator(t *testing.T) {
	relay := chatTestRelay(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host, err := StartHost(ctx, Config{RelayAddress: relay, RelayPassword: "relay-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { host.Close(); _ = host.Wait() }()

	join, err := Join(ctx, Config{RelayAddress: relay, RelayPassword: "relay-password"}, host.Code())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { join.Close(); _ = join.Wait() }()

	<-host.Events()
	<-join.Events()

	if err := host.SendTyping(true); err != nil {
		t.Fatal(err)
	}
	if err := host.SendTyping(false); err != nil {
		t.Fatal(err)
	}

	var states []bool
	for len(states) < 2 {
		select {
		case ev := <-join.Events():
			if ev.Type == PeerTyping {
				states = append(states, ev.Typing)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for typing events, got %v", states)
		}
	}
	if !states[0] || states[1] {
		t.Fatalf("expected [true false], got %v", states)
	}
}

func TestChatKeyRotationAfter20Messages(t *testing.T) {
	relay := chatTestRelay(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	host, err := StartHost(ctx, Config{RelayAddress: relay, RelayPassword: "relay-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { host.Close(); _ = host.Wait() }()

	join, err := Join(ctx, Config{RelayAddress: relay, RelayPassword: "relay-password"}, host.Code())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { join.Close(); _ = join.Wait() }()

	<-host.Events()
	<-join.Events()

	const total = 25
	for i := 1; i <= total; i++ {
		if _, err := host.SendText(fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatalf("SendText %d failed: %v", i, err)
		}
	}

	received := 0
	joinRotated := uint64(0)
	for received < total || joinRotated == 0 {
		select {
		case ev := <-join.Events():
			switch ev.Type {
			case MessageReceived:
				received++
				want := fmt.Sprintf("msg-%d", received)
				if ev.Text != want {
					t.Fatalf("message %d: want %q, got %q", received, want, ev.Text)
				}
			case KeyRotated:
				joinRotated = ev.ID
			case Failed, Closed:
				t.Fatalf("joiner ended unexpectedly: %+v", ev)
			}
		case <-ctx.Done():
			t.Fatalf("timed out: received=%d joinRotated=%d", received, joinRotated)
		}
	}

	hostRotated := uint64(0)
	acks := 0
	for acks < total || hostRotated == 0 {
		select {
		case ev := <-host.Events():
			switch ev.Type {
			case Acknowledged:
				acks++
			case KeyRotated:
				hostRotated = ev.ID
			case Failed, Closed:
				t.Fatalf("host ended unexpectedly: %+v", ev)
			}
		case <-ctx.Done():
			t.Fatalf("timed out on host: acks=%d hostRotated=%d", acks, hostRotated)
		}
	}

	if hostRotated != 1 || joinRotated != 1 {
		t.Fatalf("expected epoch 1 rotation on both peers, got host=%d join=%d", hostRotated, joinRotated)
	}
}
