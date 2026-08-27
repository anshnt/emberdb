package value

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrCorruptEncoding reports bytes that do not decode as a value.
var ErrCorruptEncoding = errors.New("emberdb: corrupt value encoding")

// Row encoding is compact rather than order-preserving: every value is written
// as a one-byte type tag followed by its payload, and integers use a varint so
// small numbers cost two bytes.

// AppendRow encodes a row onto dst and returns the extended slice.
func AppendRow(dst []byte, row []Value) []byte {
	for _, v := range row {
		dst = append(dst, byte(v.kind))
		switch v.kind {
		case TypeNull:
		case TypeInteger:
			dst = binary.AppendVarint(dst, v.num)
		case TypeReal:
			var buf [8]byte
			binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v.real))
			dst = append(dst, buf[:]...)
		default:
			dst = binary.AppendUvarint(dst, uint64(len(v.data)))
			dst = append(dst, v.data...)
		}
	}
	return dst
}

// DecodeRow decodes exactly n values from src.
func DecodeRow(src []byte, n int) ([]Value, error) {
	row := make([]Value, 0, n)
	offset := 0
	for i := 0; i < n; i++ {
		if offset >= len(src) {
			return nil, fmt.Errorf("%w: row ended after %d of %d columns", ErrCorruptEncoding, i, n)
		}
		kind := Type(src[offset])
		offset++
		switch kind {
		case TypeNull:
			row = append(row, Null())
		case TypeInteger:
			number, read := binary.Varint(src[offset:])
			if read <= 0 {
				return nil, fmt.Errorf("%w: unreadable integer in column %d", ErrCorruptEncoding, i)
			}
			offset += read
			row = append(row, Integer(number))
		case TypeReal:
			if offset+8 > len(src) {
				return nil, fmt.Errorf("%w: truncated real in column %d", ErrCorruptEncoding, i)
			}
			row = append(row, Real(math.Float64frombits(binary.LittleEndian.Uint64(src[offset:]))))
			offset += 8
		case TypeText, TypeBlob:
			length, read := binary.Uvarint(src[offset:])
			if read <= 0 {
				return nil, fmt.Errorf("%w: unreadable length in column %d", ErrCorruptEncoding, i)
			}
			offset += read
			if offset+int(length) > len(src) {
				return nil, fmt.Errorf("%w: column %d claims %d bytes", ErrCorruptEncoding, i, length)
			}
			payload := append([]byte(nil), src[offset:offset+int(length)]...)
			offset += int(length)
			row = append(row, Value{kind: kind, data: payload})
		default:
			return nil, fmt.Errorf("%w: column %d has type tag %d", ErrCorruptEncoding, i, kind)
		}
	}
	return row, nil
}

// Index keys have to sort as bytes exactly the way values sort, because the
// B+tree only knows how to compare bytes. Each encoding below is chosen so
// that memcmp order matches Compare order.
const (
	keyTagNull    = 0x00
	keyTagInteger = 0x01
	keyTagReal    = 0x02
	keyTagText    = 0x03
	keyTagBlob    = 0x04
)

// AppendKey encodes v onto dst so that byte order matches value order.
//
// Integers flip their sign bit and are written big-endian, which maps the
// signed range onto the unsigned range in order. Reals use the standard IEEE
// trick: a negative float is inverted entirely, a positive one gets its sign
// bit set. Text and blobs escape their zero bytes and end with a terminator,
// so that a key which is a prefix of another still sorts before it once
// something else is appended.
//
// Integers and reals carry different tags, so an integer sorts before a real
// in an index. That never contradicts Compare, because emberdb stores a column
// in exactly one of the two: see the coercion rules in the store package.
func AppendKey(dst []byte, v Value) []byte {
	switch v.kind {
	case TypeNull:
		return append(dst, keyTagNull)
	case TypeInteger:
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(v.num)^(1<<63))
		return append(append(dst, keyTagInteger), buf[:]...)
	case TypeReal:
		bits := math.Float64bits(v.real)
		if bits&(1<<63) != 0 {
			bits = ^bits
		} else {
			bits |= 1 << 63
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], bits)
		return append(append(dst, keyTagReal), buf[:]...)
	case TypeText:
		return appendEscaped(append(dst, keyTagText), v.data)
	default:
		return appendEscaped(append(dst, keyTagBlob), v.data)
	}
}

// appendEscaped writes a byte string so that it is self-terminating and order
// preserving: 0x00 becomes 0x00 0xFF, and the string ends with 0x00 0x00.
func appendEscaped(dst, src []byte) []byte {
	for _, b := range src {
		if b == 0x00 {
			dst = append(dst, 0x00, 0xFF)
			continue
		}
		dst = append(dst, b)
	}
	return append(dst, 0x00, 0x00)
}

// DecodeKey reads one encoded value from src and returns it with the number of
// bytes consumed.
func DecodeKey(src []byte) (Value, int, error) {
	if len(src) == 0 {
		return Null(), 0, fmt.Errorf("%w: empty index key", ErrCorruptEncoding)
	}
	switch src[0] {
	case keyTagNull:
		return Null(), 1, nil
	case keyTagInteger:
		if len(src) < 9 {
			return Null(), 0, fmt.Errorf("%w: truncated integer key", ErrCorruptEncoding)
		}
		return Integer(int64(binary.BigEndian.Uint64(src[1:]) ^ (1 << 63))), 9, nil
	case keyTagReal:
		if len(src) < 9 {
			return Null(), 0, fmt.Errorf("%w: truncated real key", ErrCorruptEncoding)
		}
		bits := binary.BigEndian.Uint64(src[1:])
		if bits&(1<<63) != 0 {
			bits &^= 1 << 63
		} else {
			bits = ^bits
		}
		return Real(math.Float64frombits(bits)), 9, nil
	case keyTagText, keyTagBlob:
		payload, read, err := decodeEscaped(src[1:])
		if err != nil {
			return Null(), 0, err
		}
		kind := TypeText
		if src[0] == keyTagBlob {
			kind = TypeBlob
		}
		return Value{kind: kind, data: payload}, read + 1, nil
	default:
		return Null(), 0, fmt.Errorf("%w: index key has tag %d", ErrCorruptEncoding, src[0])
	}
}

// decodeEscaped reverses appendEscaped.
func decodeEscaped(src []byte) ([]byte, int, error) {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); {
		if src[i] != 0x00 {
			out = append(out, src[i])
			i++
			continue
		}
		if i+1 >= len(src) {
			return nil, 0, fmt.Errorf("%w: index key ends inside an escape", ErrCorruptEncoding)
		}
		switch src[i+1] {
		case 0x00:
			return out, i + 2, nil
		case 0xFF:
			out = append(out, 0x00)
			i += 2
		default:
			return nil, 0, fmt.Errorf("%w: index key has escape 0x00 %#x", ErrCorruptEncoding, src[i+1])
		}
	}
	return nil, 0, fmt.Errorf("%w: index key has no terminator", ErrCorruptEncoding)
}
