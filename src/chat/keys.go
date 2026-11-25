package chat

import (
	"crypto/sha256"
	"errors"
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
