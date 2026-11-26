// Package chat implements croc's authenticated, ephemeral one-to-one chat protocol.
package chat

import (
	"encoding/binary"
	"errors"
	"unicode/utf8"
)

const (
	protocolVersion        = 1
	maxFrameSize           = 64 << 10
	maxTextSize            = 16 << 10
	maxRecords      uint64 = 1 << 20
	headerSize             = 10
	maxTTLSeconds   uint32 = 86400
	keyRotateEvery         = 20
	keyRotateSalt          = 32
)

type recordType byte

const (
	typeReady recordType = iota + 1
	typeText
	typeAck
	typePing
	typePong
	typeClose
	typeTTLText
	typeTyping
	typeKeyRotate
)

const (
	closeNormal byte = iota
	closeExpired
	closeKeyLimit
	closeOverloaded
)

var errProtocol = errors.New("invalid chat record")
var errKeyLimit = errors.New("chat record key limit reached")

type record struct {
	kind    recordType
	seq     uint64
	payload []byte
}

func validateRecord(r record) error {
	if r.seq >= maxRecords {
		return errKeyLimit
	}
	if r.seq == 0 && r.kind != typeReady || r.seq != 0 && r.kind == typeReady {
		return errProtocol
	}
	if r.seq == maxRecords-1 && (r.kind != typeClose || len(r.payload) != 1 || r.payload[0] != closeKeyLimit) {
		return errKeyLimit
	}
	switch r.kind {
	case typeReady:
		if len(r.payload) != 0 {
			return errProtocol
		}
	case typeText:
		if len(r.payload) == 0 || len(r.payload) > maxTextSize || !utf8.Valid(r.payload) {
			return errProtocol
		}
	case typeAck, typePing, typePong:
		if len(r.payload) != 8 {
			return errProtocol
		}
	case typeClose:
		if len(r.payload) != 1 || r.payload[0] > closeOverloaded {
			return errProtocol
		}
	case typeTTLText:
		if len(r.payload) <= 4 || len(r.payload) > 4+maxTextSize {
			return errProtocol
		}
		ttlSec := binary.BigEndian.Uint32(r.payload[:4])
		if ttlSec == 0 || ttlSec > maxTTLSeconds || !utf8.Valid(r.payload[4:]) {
			return errProtocol
		}
	case typeTyping:
		if len(r.payload) != 1 || (r.payload[0] != 0 && r.payload[0] != 1) {
			return errProtocol
		}
	case typeKeyRotate:
		if len(r.payload) != keyRotateSalt {
			return errProtocol
		}
	default:
		return errProtocol
	}
	return nil
}
