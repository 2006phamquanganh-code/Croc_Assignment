package codephrase

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestChatInvitation(t *testing.T) {
	code := "yo-yo-acid-acorn-acre-acts-ahead"
	got, err := ParseChat(" \n" + code + "\t")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("yo-yo-acidcroc-chat-v1"))
	if got.RoomName != hex.EncodeToString(digest[:]) || got.PAKEPassphrase != "acorn-acre-acts-ahead" {
		t.Fatal("incorrect room/password split")
	}
	ssh, err := ParseSSH(code)
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Parse(code)
	if err != nil {
		t.Fatal(err)
	}
	if got.RoomName == ssh.RoomName || got.RoomName == transfer.RoomName {
		t.Fatal("chat room overlaps another protocol")
	}
	for _, bad := range []string{"", "acid-acorn-acre", "acid-acorn-acre-acts-ahead-UNKNOWN", code + "-acid"} {
		if _, err := ParseChat(bad); err == nil {
			t.Fatalf("accepted invalid code %q", bad)
		}
	}
	for i := 0; i < 20; i++ {
		generated, err := GenerateChat()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseChat(generated); err != nil {
			t.Fatal(err)
		}
	}
}
