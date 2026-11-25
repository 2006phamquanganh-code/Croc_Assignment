package chat

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/schollz/croc/v11/src/comm"
	"github.com/schollz/croc/v11/src/tcp"
)

func chatTestRelay(t *testing.T) string {
	t.Helper()
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
			t.Error("relay shutdown timed out")
		}
	})
	return listener.Addr().String()
}

// chatTestProxy implements only the two tunnel protocols needed by these tests.
// A .invalid destination proves the client sends relay DNS through the proxy;
// the proxy forwards it to the controlled relay rather than using public DNS.
func chatTestProxy(t *testing.T, kind, relay, target string) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	count := new(atomic.Int32)
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer client.Close()
				stop := context.AfterFunc(ctx, func() { client.Close() })
				defer stop()
				_ = client.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(client)
				if kind == "socks5" {
					var greeting [2]byte
					if _, err := io.ReadFull(reader, greeting[:]); err != nil || greeting[0] != 5 {
						return
					}
					methods := make([]byte, int(greeting[1]))
					if _, err := io.ReadFull(reader, methods); err != nil {
						return
					}
					if _, err := client.Write([]byte{5, 0}); err != nil {
						return
					}
					var req [5]byte
					if _, err := io.ReadFull(reader, req[:]); err != nil || req[0] != 5 || req[1] != 1 || req[3] != 3 {
						return
					}
					host := make([]byte, int(req[4]))
					if _, err := io.ReadFull(reader, host); err != nil {
						return
					}
					var port [2]byte
					if _, err := io.ReadFull(reader, port[:]); err != nil {
						return
					}
					if net.JoinHostPort(string(host), strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))) != target {
						return
					}
				} else {
					req, err := http.ReadRequest(reader)
					if err != nil || req.Method != "CONNECT" || req.Host != target {
						return
					}
				}
				backend, err := (&net.Dialer{}).DialContext(ctx, "tcp", relay)
				if err != nil {
					return
				}
				defer backend.Close()
				stopBackend := context.AfterFunc(ctx, func() { backend.Close() })
				defer stopBackend()
				if kind == "socks5" {
					if _, err := client.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
						return
					}
				} else if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					return
				}
				count.Add(1)
				_ = client.SetDeadline(time.Time{})
				copied := make(chan struct{})
				go func() { io.Copy(backend, reader); backend.Close(); client.Close(); close(copied) }()
				io.Copy(client, backend)
				backend.Close()
				client.Close()
				<-copied
			}()
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); <-done; workers.Wait() })
	return listener.Addr().String(), count
}

func TestChatSessionsThroughProxies(t *testing.T) {
	for _, kind := range []string{"socks5", "http"} {
		t.Run(kind, func(t *testing.T) {
			relay := chatTestRelay(t)
			_, port, err := net.SplitHostPort(relay)
			if err != nil {
				t.Fatal(err)
			}
			target := net.JoinHostPort("chat-relay.invalid", port)
			proxy, count := chatTestProxy(t, kind, relay, target)
			oldSOCKS, oldHTTP := comm.Socks5Proxy, comm.HttpProxy
			comm.Socks5Proxy, comm.HttpProxy = "", ""
			if kind == "socks5" {
				comm.Socks5Proxy = "socks5://" + proxy
			} else {
				comm.HttpProxy = "http://" + proxy
			}
			t.Cleanup(func() { comm.Socks5Proxy, comm.HttpProxy = oldSOCKS, oldHTTP })
			cfg := Config{RelayAddress: target, RelayPassword: "relay-password"}
			host, err := StartHost(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { host.Close(); host.Wait() }()
			join, err := Join(t.Context(), cfg, host.Code())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { join.Close(); join.Wait() }()
			waitEvent(t, host, Connected)
			waitEvent(t, join, Connected)
			id, err := host.SendText("Xin chào qua " + kind)
			if err != nil {
				t.Fatal(err)
			}
			if e := waitEvent(t, join, MessageReceived); e.Text != "Xin chào qua "+kind {
				t.Fatal("proxy changed chat text")
			}
			if e := waitEvent(t, host, Acknowledged); e.ID != id {
				t.Fatal("wrong message acknowledged")
			}
			join.Close()
			if err := host.Wait(); err != nil {
				t.Fatal(err)
			}
			if err := join.Wait(); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 2 {
				t.Fatalf("expected both clients through proxy, got %d", count.Load())
			}
		})
	}
}

func TestChatRejectsThirdParticipant(t *testing.T) {
	cfg := Config{RelayAddress: chatTestRelay(t), RelayPassword: "relay-password"}
	host, err := StartHost(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { host.Close(); host.Wait() }()
	join, err := Join(t.Context(), cfg, host.Code())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { join.Close(); join.Wait() }()
	waitEvent(t, host, Connected)
	waitEvent(t, join, Connected)
	third, err := Join(t.Context(), cfg, host.Code())
	if third != nil {
		third.Close()
		third.Wait()
		t.Fatal("third participant entered the chat room")
	}
	if err == nil {
		t.Fatal("third participant was not rejected")
	}
	// A rejected third client must not evict either authenticated participant.
	id, err := join.SendText("original participants remain connected")
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, host, MessageReceived)
	if e := waitEvent(t, join, Acknowledged); e.ID != id {
		t.Fatal("original session lost its ACK")
	}
	host.Close()
	if err := host.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := join.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestChatAdmissionDeadlineClosesConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peerDone := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			peerDone <- err
			return
		}
		defer c.Close()
		_, err = io.Copy(io.Discard, c)
		peerDone <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	s, err := StartHost(ctx, Config{RelayAddress: listener.Addr().String(), RelayPassword: "private-relay-password"})
	if s != nil {
		s.Close()
		s.Wait()
		t.Fatal("incomplete admission returned a session")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("admission deadline: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "private-relay-password") {
		t.Fatal("admission error leaked password")
	}
	select {
	case err := <-peerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled admission retained the relay socket")
	}
}
