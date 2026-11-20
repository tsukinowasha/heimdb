// Package keyenc は、バイト列として比較した順序が元の値の順序と一致する符号化を提供する。
// 複数の値をつないだキーも、先頭の値から順に比較した順序になる。
package keyenc

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrCorrupt は、復号しようとしたバイト列が符号化の形式に合わないことを表す。
var ErrCorrupt = errors.New("keyenc: corrupt encoding")

const (
	// NullTag は NULL を表す。値ありの NotNullTag より前に並ぶ（MySQL の昇順では NULL が先頭）。
	NullTag byte = 0x00
	// NotNullTag は、続くバイト列に値があることを表す。
	NotNullTag byte = 0x01
)

const (
	escape     byte = 0x00
	escaped00  byte = 0xFF
	terminator byte = 0x01
)

// AppendUint64 は v をビッグエンディアンで追記する。
func AppendUint64(b []byte, v uint64) []byte {
	return binary.BigEndian.AppendUint64(b, v)
}

// DecodeUint64 は AppendUint64 で符号化した値を読み、残りのバイト列を返す。
func DecodeUint64(b []byte) (uint64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, ErrCorrupt
	}
	return binary.BigEndian.Uint64(b), b[8:], nil
}

// AppendInt64 は符号ビットを反転してビッグエンディアンで追記する。
func AppendInt64(b []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(b, uint64(v)^(1<<63))
}

// DecodeInt64 は AppendInt64 で符号化した値を読み、残りのバイト列を返す。
func DecodeInt64(b []byte) (int64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, ErrCorrupt
	}
	return int64(binary.BigEndian.Uint64(b) ^ (1 << 63)), b[8:], nil
}

// AppendBytes は 0x00 を 0x00 0xFF にエスケープし、0x00 0x01 で終端して追記する。
// 終端があるので、複数の値をつないでも ("ab", "c") < ("abc", ...) の順序が保たれる。
func AppendBytes(b []byte, s []byte) []byte {
	for _, c := range s {
		if c == escape {
			b = append(b, escape, escaped00)
		} else {
			b = append(b, c)
		}
	}
	return append(b, escape, terminator)
}

// AppendString は AppendBytes の文字列版。
func AppendString(b []byte, s string) []byte {
	return AppendBytes(b, []byte(s))
}

// DecodeBytes は AppendBytes で符号化した値を読み、残りのバイト列を返す。
func DecodeBytes(b []byte) ([]byte, []byte, error) {
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] != escape {
			out = append(out, b[i])
			continue
		}
		if i+1 >= len(b) {
			return nil, nil, ErrCorrupt
		}
		switch b[i+1] {
		case escaped00:
			out = append(out, escape)
			i++
		case terminator:
			if out == nil {
				out = []byte{}
			}
			return out, b[i+2:], nil
		default:
			return nil, nil, ErrCorrupt
		}
	}
	return nil, nil, ErrCorrupt
}

// AppendInvertedTime は時刻を反転（math.MaxUint64 - ts）して追記する。
// 同じキーのバージョンが新しい順に並ぶ。
func AppendInvertedTime(b []byte, ts uint64) []byte {
	return AppendUint64(b, math.MaxUint64-ts)
}

// DecodeInvertedTime は AppendInvertedTime で符号化した時刻を読み、残りのバイト列を返す。
func DecodeInvertedTime(b []byte) (uint64, []byte, error) {
	v, rest, err := DecodeUint64(b)
	if err != nil {
		return 0, nil, err
	}
	return math.MaxUint64 - v, rest, nil
}

// AppendNull は列の値としての NULL を追記する。
func AppendNull(b []byte) []byte {
	return append(b, NullTag)
}

// AppendNotNull は、続けて値を追記する前の「値あり」の印を追記する。
func AppendNotNull(b []byte) []byte {
	return append(b, NotNullTag)
}

// DecodeNullTag は列の値の先頭の印を読む。NULL なら isNull が true になる。
func DecodeNullTag(b []byte) (isNull bool, rest []byte, err error) {
	if len(b) < 1 {
		return false, nil, ErrCorrupt
	}
	switch b[0] {
	case NullTag:
		return true, b[1:], nil
	case NotNullTag:
		return false, b[1:], nil
	default:
		return false, nil, ErrCorrupt
	}
}

// PrefixEnd は、prefix で始まるすべてのキーより大きい最小のキーを返す。
// 範囲走査の上限に使う。prefix がすべて 0xFF なら nil（上限なし）を返す。
func PrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		end[i]++
		if end[i] != 0 {
			return end[:i+1]
		}
	}
	return nil
}
