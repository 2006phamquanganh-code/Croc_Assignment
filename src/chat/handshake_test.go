package chat

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/schollz/croc/v11/src/codephrase"
	"github.com/schollz/croc/v11/src/comm"
	"github.com/schollz/croc/v11/src/compress"
	"github.com/schollz/croc/v11/src/message"
)

func TestHandshakeAndTraffic(t *testing.T) {
	for _, wrongPassword := range []bool{false, true} {
		t.Run(map[bool]string{false: "agreement", true: "wrong-password"}[wrongPassword], func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			host, join := comm.New(a), comm.New(b)
			components, err := codephrase.ParseChat("acid-acorn-acre-acts-ahead-alien")
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				send, receive *recordCipher
				err           error
			}
			ch := make(chan result, 1)
			go func() {
				s, r, e := handshake(host, components, true, time.Second, time.Second)
				if e != nil {
					host.Close()
				}
				ch <- result{s, r, e}
			}()
			other := components
			if wrongPassword {
				other.PAKEPassphrase = "wrong password"
			}
			s, r, e := handshake(join, other, false, time.Second, time.Second)
			if e != nil {
				join.Close()
			}
			h := <-ch
			if wrongPassword {
				if h.err == nil || e == nil {
					t.Fatal("wrong password authenticated")
				}
				return
			}
			if h.err != nil || e != nil {
				t.Fatalf("handshake failed: %v %v", h.err, e)
			}
			for _, pair := range [][2]*recordCipher{{h.send, r}, {s, h.receive}} {
				ready, err := pair[0].seal(typeReady, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pair[1].open(ready); err != nil {
					t.Fatal(err)
				}
				text, err := pair[0].seal(typeText, []byte("xin chào"))
				if err != nil {
					t.Fatal(err)
				}
				got, err := pair[1].open(text)
				if err != nil || string(got.payload) != "xin chào" {
					t.Fatalf("traffic mismatch: %v", err)
				}
			}
		})
	}
}

func TestStrictHandshakeJSON(t *testing.T) {
	valid := `{"t":"pake","v":2,"b":"AQ==","b2":"cDI1Ng==","f":["chat-rendezvous-v1"]}`
	for _, plain := range []string{
		valid[:len(valid)-1] + `,"v":2}`,
		valid[:len(valid)-1] + `,"unknown":1}`,
		valid + ` {}`,
		`{"t":"pake","v":2,"b":[1],"b2":"cDI1Ng==","f":["chat-rendezvous-v1"]}`,
		`{"t":"pake","v":2,"b":null,"b2":"cDI1Ng==","f":["chat-rendezvous-v1"]}`,
		`{"t":"pake","v":2,"b":"AQ=="}`,
	} {
		if _, err := decodeHandshake(compress.Compress([]byte(plain)), false, false); err == nil {
			t.Fatal("accepted malformed handshake")
		}
	}
	if _, err := decodeHandshake(compress.Compress([]byte(valid)), false, false); err != nil {
		t.Fatal(err)
	}
	encoded, err := message.Encode(nil, message.Message{Type: message.TypePAKE, Version: 2, Bytes: []byte{1}, Bytes2: []byte(chatCurve), Features: []string{chatMarker}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeHandshake(encoded, false, false)
	if err != nil || !bytes.Equal(got.Bytes, []byte{1}) {
		t.Fatalf("existing encoder interop: %v", err)
	}
}

func FuzzHandshakeDecode(f *testing.F) {
	f.Add(compress.Compress([]byte(`{"t":"pake","v":2,"b":"AQ==","b2":"cDI1Ng==","f":["chat-rendezvous-v1"]}`)))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, mode := range [][2]bool{{false, false}, {true, false}, {false, true}} {
			m, err := decodeHandshake(body, mode[0], mode[1])
			if err == nil && (m.Version != 2 || len(m.Bytes) == 0) {
				t.Fatal("accepted invalid handshake")
			}
		}
	})
}

func TestHandshakeRejectsWrongParameters(t *testing.T) {
	for _, test := range []struct {
		name                   string
		m                      message.Message
		response, confirmation bool
	}{
		{"version", message.Message{Type: message.TypePAKE, Version: 1, Bytes: []byte{1}, Bytes2: []byte(chatCurve), Features: []string{chatMarker}}, false, false},
		{"curve", message.Message{Type: message.TypePAKE, Version: 2, Bytes: []byte{1}, Bytes2: []byte("p384"), Features: []string{chatMarker}}, false, false},
		{"marker", message.Message{Type: message.TypePAKE, Version: 2, Bytes: []byte{1}, Bytes2: []byte(chatCurve), Features: []string{"ssh-rendezvous-v1"}}, false, false},
		{"payload", message.Message{Type: message.TypePAKE, Version: 2, Bytes: make([]byte, 4097), Bytes2: []byte(chatCurve), Features: []string{chatMarker}}, false, false},
		{"salt", message.Message{Type: message.TypePAKE, Version: 2, Bytes: []byte{1}, Bytes2: make([]byte, 31), Features: []string{chatMarker}}, true, false},
		{"confirmation", message.Message{Type: message.TypePAKEConfirm, Version: 2, Bytes: make([]byte, 31)}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := message.Encode(nil, test.m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeHandshake(encoded, test.response, test.confirmation); err == nil {
				t.Fatal("accepted wrong handshake parameters")
			}
		})
	}
}
func TestRelayKeepalivesDoNotExtendHandshakeDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	reader, writer := comm.New(a), comm.New(b)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if err := writer.Send([]byte{1}); err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(30 * time.Millisecond)
	if _, err := receiveHandshake(reader, deadline, false, false); err == nil {
		t.Fatal("accepted keepalive as handshake")
	}
	if time.Now().Before(deadline) {
		t.Fatal("did not wait for handshake deadline")
	}
	reader.Close()
	<-done
}
