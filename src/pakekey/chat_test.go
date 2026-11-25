package pakekey

import (
	"bytes"
	"testing"
)

func TestChatAgreementAndPurposeSeparation(t *testing.T) {
	for _, purposeB := range []string{PurposeChatV1, PurposeTransfer, PurposeSSH} {
		t.Run(purposeB, func(t *testing.T) {
			a, err := Init([]byte("chat password"), 0, "p256", PurposeChatV1, "room")
			if err != nil {
				t.Fatal(err)
			}
			b, err := Init([]byte("chat password"), 1, "p256", purposeB, "room")
			if err != nil {
				t.Fatal(err)
			}
			x := append([]byte(nil), a.Bytes()...)
			if err := b.Update(x); err != nil {
				t.Fatal(err)
			}
			y := b.Bytes()
			if err := a.Update(y); err != nil {
				t.Fatal(err)
			}
			sa, err := a.SessionKey()
			if err != nil {
				t.Fatal(err)
			}
			sb, err := b.SessionKey()
			if err != nil {
				t.Fatal(err)
			}
			ctx := Context{Purpose: PurposeChatV1, Room: "room", Curve: "p256", Initiator: x, Responder: y, Salt: bytes.Repeat([]byte{7}, SaltSize)}
			ka, err := Derive(sa, ctx)
			if err != nil {
				t.Fatal(err)
			}
			ctx.Purpose = purposeB
			kb, err := Derive(sb, ctx)
			if err != nil {
				t.Fatal(err)
			}
			matches := Confirm(ka.ConfirmationA, kb.ConfirmationA) && Confirm(ka.ConfirmationB, kb.ConfirmationB) && bytes.Equal(ka.EncryptionKey, kb.EncryptionKey)
			if matches != (purposeB == PurposeChatV1) {
				t.Fatal("incorrect purpose isolation")
			}
		})
	}
}
