package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/schollz/croc/v11/src/codephrase"
	"github.com/schollz/peerdiscovery"
)

func TestChatLocalDiscoveryPayloadParsing(t *testing.T) {
	discoveries := []peerdiscovery.Discovered{
		{Address: "192.168.1.10", Payload: []byte("croc9009")},          // file-transfer broadcast, must be ignored
		{Address: "192.168.1.11", Payload: []byte("croc-chat:54321")},   // valid IPv4
		{Address: "192.168.1.11", Payload: []byte("croc-chat:54321")},   // duplicate
		{Address: "192.168.1.12", Payload: []byte("croc-chat:invalid")}, // invalid port
		{Address: "192.168.1.13", Payload: []byte("croc-chat:0")},       // invalid port
		{Address: "fe80::1", Payload: []byte("croc-chat:41234")},        // valid IPv6
	}
	got := parseChatDiscoveryAddresses(discoveries)
	if len(got) != 2 || got[0] != "192.168.1.11:54321" || got[1] != "[fe80::1]:41234" {
		t.Fatalf("unexpected discovered addresses: %v", got)
	}
}

func TestChatTransportStatsFormatting(t *testing.T) {
	if got := formatTransportStats(true, "192.168.1.5:54321"); got != "Direct LAN (192.168.1.5:54321)" {
		t.Fatalf("unexpected LAN stats: %q", got)
	}
	if got := formatTransportStats(false, "croc.schollz.com:9009"); got != "Relay Server (croc.schollz.com:9009)" {
		t.Fatalf("unexpected Relay stats: %q", got)
	}
}

func TestChatLocalDirectSession(t *testing.T) {
	origDiscover := chatPeerDiscover
	t.Cleanup(func() { chatPeerDiscover = origDiscover })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	code, err := codephrase.GenerateChat()
	if err != nil {
		t.Fatal(err)
	}
	components, err := codephrase.ParseChat(code)
	if err != nil {
		t.Fatal(err)
	}

	// Stub UDP discovery to return the host's local relay port on 127.0.0.1.
	portCh := make(chan string, 1)
	chatPeerDiscover = func(settings ...peerdiscovery.Settings) ([]peerdiscovery.Discovered, error) {
		if len(settings) == 0 {
			return nil, nil
		}
		payload := string(settings[0].Payload)
		if strings.HasPrefix(payload, chatLocalDiscoveryPrefix) {
			select {
			case portCh <- payload:
			default:
			}
			if settings[0].StopChan != nil {
				<-settings[0].StopChan
			}
			return nil, nil
		}
		// Joiner probe: return discovered host payload on loopback.
		select {
		case p := <-portCh:
			// Put back in case IPv6 probe also reads.
			select {
			case portCh <- p:
			default:
			}
			return []peerdiscovery.Discovered{{Address: "127.0.0.1", Payload: []byte(p)}}, nil
		case <-time.After(200 * time.Millisecond):
			return nil, nil
		}
	}

	localHost, err := startLocalHostRelay(ctx, components.RoomName, "local-pass", "")
	if err != nil {
		t.Fatal(err)
	}
	hostSess := newSessionWithLocal(ctx, nil, localHost, components, true, code, localHost.addr, defaults())
	defer func() {
		hostSess.Close()
		_ = hostSess.Wait()
	}()

	joinSess, err := Join(ctx, Config{
		RelayPassword: "local-pass",
		OnlyLocal:     true,
	}, code)
	if err != nil {
		t.Fatalf("local join failed: %v", err)
	}
	defer func() {
		joinSess.Close()
		_ = joinSess.Wait()
	}()

	waitConnected := func(s *Session, name string) {
		t.Helper()
		select {
		case ev := <-s.Events():
			if ev.Type != Connected {
				t.Fatalf("%s expected Connected, got %+v", name, ev)
			}
		case <-ctx.Done():
			t.Fatalf("%s timed out waiting for Connected", name)
		}
	}

	waitConnected(hostSess, "host")
	waitConnected(joinSess, "joiner")

	if !hostSess.IsLocal() || !joinSess.IsLocal() {
		t.Fatalf("expected both sessions to report IsLocal=true (host=%v, joiner=%v)", hostSess.IsLocal(), joinSess.IsLocal())
	}
	if !strings.Contains(hostSess.TransportStats(), "Direct LAN") || !strings.Contains(joinSess.TransportStats(), "Direct LAN") {
		t.Fatalf("unexpected transport stats: host=%q joiner=%q", hostSess.TransportStats(), joinSess.TransportStats())
	}

	if _, err := hostSess.SendText("Xin chào qua LAN!"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-joinSess.Events():
			if ev.Type == MessageReceived {
				if ev.Text != "Xin chào qua LAN!" {
					t.Fatalf("unexpected text: %q", ev.Text)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for LAN message")
		}
	}
}
