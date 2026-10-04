// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"fmt"
	"hash"
)

// NewWholeHashState returns the serialized initial state of the resumable
// whole-archive SHA-256. The state is persisted after every committed
// segment so an interrupted upload can continue hashing where it stopped.
func NewWholeHashState() []byte {
	state, err := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		panic(fmt.Sprintf("transport: sha256 state not marshalable: %v", err))
	}
	return state
}

func restoreWholeHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
		return nil, fmt.Errorf("transport: restore whole-archive hash state: %w", err)
	}
	return h, nil
}

func saveWholeHash(h hash.Hash) ([]byte, error) {
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("transport: save whole-archive hash state: %w", err)
	}
	return state, nil
}

// WholeHashSum returns the hex SHA-256 for a serialized state, i.e. the
// archive digest once every segment has been fed in order.
func WholeHashSum(state []byte) (string, error) {
	h, err := restoreWholeHash(state)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
