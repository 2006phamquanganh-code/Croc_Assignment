package chat

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/schollz/croc/v11/src/codephrase"
	"github.com/schollz/croc/v11/src/comm"
	"github.com/schollz/croc/v11/src/compress"
	"github.com/schollz/croc/v11/src/message"
	"github.com/schollz/croc/v11/src/pakekey"
)

const chatMarker = "chat-rendezvous-v1"
const chatCurve = "p256"

var errHandshake = errors.New("chat authentication failed")

// decodeHandshake checks the JSON keys before unmarshal can discard duplicates.
func decodeHandshake(body []byte, response, confirmation bool) (message.Message, error) {
	plain, err := compress.Decompress(body, 16<<10)
	if err != nil {
		return message.Message{}, errHandshake
	}
	decoder := json.NewDecoder(bytes.NewReader(plain))
	tok, err := decoder.Token()
	if err != nil || tok != json.Delim('{') {
		return message.Message{}, errHandshake
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		tok, err = decoder.Token()
		if err != nil {
			return message.Message{}, errHandshake
		}
		key, ok := tok.(string)
		if !ok {
			return message.Message{}, errHandshake
		}
		if _, duplicate := fields[key]; duplicate {
			return message.Message{}, errHandshake
		}
		if key != "t" && key != "v" && key != "b" && (confirmation || key != "b2" && key != "f") {
			return message.Message{}, errHandshake
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return message.Message{}, errHandshake
		}
		fields[key] = value
	}
	if _, err = decoder.Token(); err != nil {
		return message.Message{}, errHandshake
	}
	if _, err = decoder.Token(); err != io.EOF {
		return message.Message{}, errHandshake
	}
	expected := 5
	if confirmation {
		expected = 3
	}
	if len(fields) != expected {
		return message.Message{}, errHandshake
	}
	// encoding/json also accepts numeric arrays for []byte; the wire schema
	// requires Base64 JSON strings, so reject that alternate representation.
	for _, key := range []string{"b", "b2"} {
		if value, ok := fields[key]; ok {
			value = bytes.TrimSpace(value)
			if len(value) == 0 || value[0] != '"' {
				return message.Message{}, errHandshake
			}
		}
	}
	var m message.Message
	if json.Unmarshal(plain, &m) != nil || m.Version != pakekey.ProtocolVersion {
		return message.Message{}, errHandshake
	}
	if confirmation {
		if m.Type != message.TypePAKEConfirm || len(m.Bytes) != 32 {
			return message.Message{}, errHandshake
		}
	} else {
		if m.Type != message.TypePAKE || len(m.Bytes) == 0 || len(m.Bytes) > 4096 || len(m.Features) != 1 || m.Features[0] != chatMarker {
			return message.Message{}, errHandshake
		}
		if response {
			if len(m.Bytes2) != pakekey.SaltSize {
				return message.Message{}, errHandshake
			}
		} else if string(m.Bytes2) != chatCurve {
			return message.Message{}, errHandshake
		}
	}
	return m, nil
}

func receiveHandshake(c *comm.Comm, deadline time.Time, response, confirmation bool) (message.Message, error) {
	for {
		body, err := c.ReceiveWithDeadlineLimit(deadline, maxFrameSize)
		if err != nil {
			return message.Message{}, err
		}
		if bytes.Equal(body, []byte{1}) {
			continue
		}
		return decodeHandshake(body, response, confirmation)
	}
}

func sendHandshake(c *comm.Comm, deadline time.Time, m message.Message) error {
	if err := c.Connection().SetWriteDeadline(deadline); err != nil {
		return err
	}
	return message.Send(c, nil, m)
}

// handshake authenticates a fresh connection; caller owns cancellation and must
// close the connection on any error. READY is performed by the session writer.
func handshake(c *comm.Comm, components codephrase.ChatComponents, host bool, wait, auth time.Duration) (send, receive *recordCipher, err error) {
	return handshakeWithFirst(c, components, host, nil, wait, auth)
}

func handshakeWithFirst(c *comm.Comm, components codephrase.ChatComponents, host bool, preReceived *message.Message, wait, auth time.Duration) (send, receive *recordCipher, err error) {
	role := 0
	if host {
		role = 1
	}
	peer, err := pakekey.Init([]byte(components.PAKEPassphrase), role, chatCurve, pakekey.PurposeChatV1, components.RoomName)
	if err != nil {
		return nil, nil, errHandshake
	}
	deadline := time.Now().Add(auth)
	var x, y, salt []byte
	if host {
		var first message.Message
		if preReceived != nil {
			first = *preReceived
		} else {
			var readErr error
			first, readErr = receiveHandshake(c, time.Now().Add(wait), false, false)
			if readErr != nil {
				return nil, nil, readErr
			}
		}
		deadline = time.Now().Add(auth)
		x = first.Bytes
		if peer.Update(x) != nil {
			return nil, nil, errHandshake
		}
		y = append([]byte(nil), peer.Bytes()...)
		salt = make([]byte, pakekey.SaltSize)
		if _, err = rand.Read(salt); err != nil {
			return nil, nil, err
		}
	} else {
		x = append([]byte(nil), peer.Bytes()...)
		if err = sendHandshake(c, deadline, message.Message{Type: message.TypePAKE, Version: pakekey.ProtocolVersion, Bytes: x, Bytes2: []byte(chatCurve), Features: []string{chatMarker}}); err != nil {
			return nil, nil, err
		}
		response, readErr := receiveHandshake(c, deadline, true, false)
		if readErr != nil {
			return nil, nil, readErr
		}
		y, salt = response.Bytes, response.Bytes2
		if peer.Update(y) != nil {
			return nil, nil, errHandshake
		}
	}
	shared, err := peer.SessionKey()
	if err != nil {
		return nil, nil, errHandshake
	}
	defer clear(shared)
	keys, err := pakekey.Derive(shared, pakekey.Context{Purpose: pakekey.PurposeChatV1, Room: components.RoomName, Curve: chatCurve, Initiator: x, Responder: y, Salt: salt})
	if err != nil {
		return nil, nil, errHandshake
	}
	defer clear(keys.EncryptionKey)
	defer clear(keys.ConfirmationA)
	defer clear(keys.ConfirmationB)
	if host {
		if err = sendHandshake(c, deadline, message.Message{Type: message.TypePAKE, Version: pakekey.ProtocolVersion, Bytes: y, Bytes2: salt, Features: []string{chatMarker}}); err != nil {
			return nil, nil, err
		}
	} else {
		if err = sendHandshake(c, deadline, message.Message{Type: message.TypePAKEConfirm, Version: pakekey.ProtocolVersion, Bytes: keys.ConfirmationA}); err != nil {
			return nil, nil, err
		}
	}
	confirmation, err := receiveHandshake(c, deadline, false, true)
	if err != nil {
		return nil, nil, err
	}
	expected := keys.ConfirmationB
	if host {
		expected = keys.ConfirmationA
	}
	if !pakekey.Confirm(expected, confirmation.Bytes) {
		return nil, nil, errHandshake
	}
	if host {
		if err = sendHandshake(c, deadline, message.Message{Type: message.TypePAKEConfirm, Version: pakekey.ProtocolVersion, Bytes: keys.ConfirmationB}); err != nil {
			return nil, nil, err
		}
	}
	hj, jh, err := deriveTrafficKeys(keys.EncryptionKey, salt)
	if err != nil {
		return nil, nil, err
	}
	defer clear(hj)
	defer clear(jh)
	if !host {
		hj, jh = jh, hj
	}
	send, err = newRecordCipher(hj)
	if err != nil {
		return nil, nil, err
	}
	receive, err = newRecordCipher(jh)
	return
}
