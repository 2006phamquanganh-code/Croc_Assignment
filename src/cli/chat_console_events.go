package cli

import (
	"encoding/binary"
	"unicode/utf16"
	"unicode/utf8"
)

// chatConsoleKeys decodes Windows KEY_EVENT_RECORD values without retaining an
// unbounded expansion of a repeated key. It is independent of OS handles so the
// Unicode and repeat semantics can also be tested on non-Windows builders.
type chatConsoleKeys struct {
	high      uint16
	repeated  rune
	remaining uint16
}

func (keys *chatConsoleKeys) next() []byte {
	if keys.remaining == 0 {
		return nil
	}
	keys.remaining--
	return utf8.AppendRune(nil, keys.repeated)
}
func (keys *chatConsoleKeys) event(record [20]byte) []byte {
	if binary.LittleEndian.Uint16(record[:2]) != 1 || binary.LittleEndian.Uint32(record[4:8]) == 0 {
		return nil
	}
	ch := binary.LittleEndian.Uint16(record[14:16])
	if ch == 0 {
		return nil
	}
	if ch >= 0xd800 && ch <= 0xdbff {
		keys.high = ch
		return nil
	}
	r := rune(ch)
	if ch >= 0xdc00 && ch <= 0xdfff && keys.high != 0 {
		r = utf16.DecodeRune(rune(keys.high), rune(ch))
	}
	keys.high = 0
	repeat := binary.LittleEndian.Uint16(record[8:10])
	if repeat > 1 {
		keys.repeated = r
		keys.remaining = repeat - 1
	}
	return utf8.AppendRune(nil, r)
}
