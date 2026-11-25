package chat

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"

	"github.com/schollz/croc/v11/src/crypt"
)

// recordCipher has one owner (reader or writer); it is never used concurrently.
// Any I/O or decode error must terminate its session, without retrying a record.
type recordCipher struct {
	aead cipher.AEAD
	next uint64
}

func newRecordCipher(key []byte) (*recordCipher, error) {
	if len(key) != 32 {
		return nil, errors.New("chat requires a 32-byte key")
	}
	aead, err := crypt.NewAESGCM(key)
	if err != nil {
		return nil, err
	}
	return &recordCipher{aead: aead}, nil
}

func (c *recordCipher) seal(kind recordType, payload []byte) ([]byte, error) {
	r := record{kind: kind, seq: c.next, payload: payload}
	if err := validateRecord(r); err != nil {
		return nil, err
	}
	plain := make([]byte, headerSize+len(payload))
	plain[0] = protocolVersion
	plain[1] = byte(kind)
	binary.BigEndian.PutUint64(plain[2:headerSize], r.seq)
	copy(plain[headerSize:], payload)
	defer clear(plain)
	body, err := crypt.EncryptAEAD(plain, c.aead)
	if err == nil {
		c.next++
	}
	return body, err
}

func (c *recordCipher) open(body []byte) (record, error) {
	if len(body) < c.aead.NonceSize()+c.aead.Overhead()+headerSize || len(body) > maxFrameSize {
		return record{}, errProtocol
	}
	plain, err := crypt.DecryptAEAD(body, c.aead)
	if err != nil {
		return record{}, errProtocol
	}
	defer clear(plain)
	if plain[0] != protocolVersion {
		return record{}, errProtocol
	}
	r := record{kind: recordType(plain[1]), seq: binary.BigEndian.Uint64(plain[2:headerSize]), payload: plain[headerSize:]}
	if r.seq != c.next {
		return record{}, errProtocol
	}
	if err := validateRecord(r); err != nil {
		return record{}, err
	}
	r.payload = append([]byte(nil), r.payload...)
	c.next++
	return r, nil
}
