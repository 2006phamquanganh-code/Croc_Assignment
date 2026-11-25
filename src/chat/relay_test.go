package chat

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/schollz/croc/v11/src/codephrase"
	"github.com/schollz/croc/v11/src/tcp"
)

func TestHandshakeThroughExistingRelay(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- tcp.RunWithListener(listener, "relay-password", tcp.WithCtx(ctx), tcp.WithLogLevel("error"), tcp.WithBanner("9009"))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("relay did not stop")
		}
	})
	components, err := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
	if err != nil {
		t.Fatal(err)
	}
	host, _, _, _, err := tcp.ConnectToTCPServerControlContext(ctx, listener.Addr().String(), "relay-password", components.RoomName, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	type result struct {
		receive *recordCipher
		err     error
	}
	ready := make(chan result, 1)
	go func() {
		_, r, e := handshake(host, components, true, 3*time.Second, time.Second)
		ready <- result{r, e}
	}()
	join, _, _, _, err := tcp.ConnectToTCPServerControlContext(ctx, listener.Addr().String(), "relay-password", components.RoomName, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer join.Close()
	send, _, err := handshake(join, components, false, 3*time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h := <-ready
	if h.err != nil {
		t.Fatal(h.err)
	}
	for _, out := range []record{{kind: typeReady}, {kind: typeText, payload: []byte("Chào qua relay")}} {
		body, err := send.seal(out.kind, out.payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := join.Send(body); err != nil {
			t.Fatal(err)
		}
		body, err = host.ReceiveWithDeadlineLimit(time.Now().Add(time.Second), maxFrameSize)
		if err != nil {
			t.Fatal(err)
		}
		got, err := h.receive.open(body)
		if err != nil {
			t.Fatal(err)
		}
		if got.kind != out.kind || string(got.payload) != string(out.payload) {
			t.Fatal("relay changed record")
		}
	}
}
