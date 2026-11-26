package chat

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// deriveTrafficKeys uses separate HKDF invocations and fixed directional labels.
func deriveTrafficKeys(root, salt []byte) (hostToJoiner, joinerToHost []byte, err error) {
	if len(root) != 32 || len(salt) != 32 {
		return nil, nil, errors.New("invalid chat key material size")
	}
	derive := func(label string) ([]byte, error) {
		key := make([]byte, 32)
		_, err := io.ReadFull(hkdf.New(sha256.New, root, salt, []byte(label)), key)
		return key, err
	}
	hostToJoiner, err = derive("croc/chat/v1/host-to-joiner")
	if err != nil {
		return nil, nil, err
	}
	joinerToHost, err = derive("croc/chat/v1/joiner-to-host")
	return
}

// deriveRotatedKey ratchets a 32-byte directional AES-256-GCM traffic key
// forward using HKDF-SHA256 with a fresh 32-byte salt and monotonic epoch.
func deriveRotatedKey(currentKey, salt []byte, epoch uint64) ([]byte, error) {
	if len(currentKey) != 32 || len(salt) != keyRotateSalt {
		return nil, errors.New("invalid key rotation material size")
	}
	info := fmt.Sprintf("croc/chat/v1/rotate/%d", epoch)
	nextKey := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, currentKey, salt, []byte(info)), nextKey); err != nil {
		clear(nextKey)
		return nil, err
	}
	return nextKey, nil
}
