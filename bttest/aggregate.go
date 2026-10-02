// SPDX-License-Identifier: Apache-2.0

package bttest

import "encoding/binary"

func mergeMin(existing, newVal []byte) []byte {
	return encodeInt64(min(decodeInt64(existing), decodeInt64(newVal)))
}

func mergeMax(existing, newVal []byte) []byte {
	return encodeInt64(max(decodeInt64(existing), decodeInt64(newVal)))
}

func decodeInt64(b []byte) int64 {
	return int64(binary.BigEndian.Uint64(b))
}

func encodeInt64(v int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(v))
}
