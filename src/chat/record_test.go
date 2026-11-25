package chat

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/schollz/croc/v11/src/crypt"
)

func testCipher(t *testing.T, key byte) *recordCipher {
	t.Helper()
	c, err := newRecordCipher(bytes.Repeat([]byte{key}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDirectionalKeys(t *testing.T) {
	a, b, err := deriveTrafficKeys(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("directional keys match")
	}
	// Fixed expected values computed independently with Python hashlib/hmac.
	wantA := "d71efe27c1dd47e6531a58811ae8fb0b327efe99ce4790297c050713978410e9"
	wantB := "242413ebdd560162a029a16f5e66c5a06215f318e23e0507444a219a0f5fd6fa"
	if hex.EncodeToString(a) != wantA || hex.EncodeToString(b) != wantB {
		t.Fatalf("unexpected key derivation: %x %x", a, b)
	}
	if _, _, err := deriveTrafficKeys(nil, make([]byte, 32)); err == nil {
		t.Fatal("accepted absent root")
	}
}

func TestReadyRecordVector(t *testing.T) {
	// Generated independently with Python cryptography AESGCM: key=0x01*32,
	// nonce=00..0b, plaintext=version 1, READY 1, uint64 seq 0, no AAD.
	body, err := hex.DecodeString("000102030405060708090a0bbaa7952620fe0679fe6a8eebb99c5331fb073233bcf59a45ce32")
	if err != nil {
		t.Fatal(err)
	}
	r, err := testCipher(t, 1).open(body)
	if err != nil || r.kind != typeReady || r.seq != 0 || len(r.payload) != 0 {
		t.Fatalf("READY vector mismatch: %+v %v", r, err)
	}
}

func TestRecordRoundTripAndReplay(t *testing.T) {
	send, recv := testCipher(t, 1), testCipher(t, 1)
	ready, err := send.seal(typeReady, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 38 {
		t.Fatalf("READY body size %d", len(ready))
	}
	if _, err := recv.open(ready); err != nil {
		t.Fatal(err)
	}
	body, err := send.seal(typeText, []byte("Chào bạn 👋"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := recv.open(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.payload) != "Chào bạn 👋" || r.seq != 1 || r.kind != typeText {
		t.Fatal("wrong decoded record")
	}
	if _, err := recv.open(body); err == nil {
		t.Fatal("accepted replay")
	}
}

func TestRejectCorruptedRecords(t *testing.T) {
	send := testCipher(t, 1)
	body, err := send.seal(typeReady, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range body {
		bad := append([]byte(nil), body...)
		bad[i] ^= 1
		recv := testCipher(t, 1)
		if _, err := recv.open(bad); err == nil {
			t.Fatalf("accepted corruption at byte %d", i)
		}
		if recv.next != 0 {
			t.Fatal("advanced counter on failed authentication")
		}
	}
	if _, err := testCipher(t, 2).open(body); err == nil {
		t.Fatal("accepted other key/direction")
	}
}

func TestRecordSchemaAndLimits(t *testing.T) {
	for _, r := range []record{
		{typeText, 0, []byte("hello")}, {typeReady, 1, nil}, {typeText, 1, nil},
		{typeText, 1, []byte{255}}, {typeText, 1, make([]byte, maxTextSize+1)},
		{typeAck, 1, make([]byte, 7)}, {typePing, 1, nil}, {typePong, 1, nil},
		{typeClose, 1, []byte{4}}, {recordType(99), 1, nil},
		{typeText, maxRecords - 1, []byte("last")}, {typeClose, maxRecords, []byte{closeKeyLimit}},
	} {
		if err := validateRecord(r); err == nil {
			t.Fatalf("accepted invalid record %+v", r)
		}
	}
	c := testCipher(t, 1)
	c.next = maxRecords - 1
	if _, err := c.seal(typeClose, []byte{closeKeyLimit}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.seal(typeClose, []byte{closeKeyLimit}); err == nil {
		t.Fatal("exceeded key budget")
	}
	if _, err := testCipher(t, 1).open(make([]byte, maxFrameSize+1)); err == nil {
		t.Fatal("accepted oversized frame")
	}
}

func TestRejectAuthenticatedWrongVersionAndSequence(t *testing.T) {
	for _, version := range []byte{1, 2} {
		for _, seq := range []uint64{0, 1} {
			if version == 1 && seq == 0 {
				continue
			}
			recv := testCipher(t, 1)
			plain := make([]byte, headerSize)
			plain[0] = version
			plain[1] = byte(typeReady)
			binary.BigEndian.PutUint64(plain[2:], seq)
			body, err := crypt.EncryptAEAD(plain, recv.aead)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := recv.open(body); err == nil {
				t.Fatal("accepted wrong version/sequence")
			}
		}
	}
}

func FuzzRecordOpen(f *testing.F) {
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{1}, 38))
	f.Fuzz(func(t *testing.T, body []byte) {
		c := testCipher(t, 1)
		r, err := c.open(body)
		if err == nil && (r.kind != typeReady || r.seq != 0 || len(r.payload) != 0) {
			t.Fatal("first record was not READY")
		}
	})
}
