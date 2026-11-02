package engine

import (
	"sync"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
)

// x/text の ShiftJIS は WHATWG 準拠で、CP932 の外字領域 (F040-F9FC) と
// 0x80/0xA0/0xFD-0xFF を扱えない。入力データを落とさないよう、不足分を補った
// 表を初回利用時に組み立てる。

const (
	cp932LeadMin  = 0x81
	cp932LeadMax  = 0xFC
	cp932DecCount = (cp932LeadMax - cp932LeadMin + 1) * 256
)

var (
	cp932DecOnce sync.Once
	cp932Dec     []uint16 // [(lead-0x81)<<8|trail] -> UCS-2 (0 は未定義)

	cp932EncOnce sync.Once
	cp932Enc     []uint16 // [rune] -> CP932 (0 は未定義, 0x100 未満は1バイト)
)

// cp932EncExtra は復号には現れないが符号化できる文字 (Unicode 間の揺れの吸収)。
var cp932EncExtra = map[rune]uint16{
	0x00A2: 0x8191, 0x00A3: 0x8192, 0x00AC: 0x81CA,
	0x2016: 0x8161, 0x2212: 0x817C, 0x301C: 0x8160,
}

func isUserDefinedLead(c int) bool { return c >= 0xF0 && c <= 0xF9 }

func userDefinedRune(lead, trail int) uint16 {
	t := trail - 0x41
	if trail < 0x80 {
		t = trail - 0x40
	}
	return uint16(0xE000 + 188*(lead-0xF0) + t)
}

func buildCP932Dec() {
	cp932Dec = make([]uint16, cp932DecCount)
	dec := japanese.ShiftJIS.NewDecoder()
	var buf [8]byte
	for lead := cp932LeadMin; lead <= cp932LeadMax; lead++ {
		for trail := 0x40; trail <= 0xFC; trail++ {
			if !isTrailByte(byte(trail)) {
				continue
			}
			idx := (lead-cp932LeadMin)<<8 | trail
			if isUserDefinedLead(lead) {
				cp932Dec[idx] = userDefinedRune(lead, trail)
				continue
			}
			nDst, nSrc, err := dec.Transform(buf[:], []byte{byte(lead), byte(trail)}, true)
			dec.Reset()
			r, n := utf8.DecodeRune(buf[:nDst])
			if err == nil && nSrc == 2 && n == nDst && r != utf8.RuneError {
				cp932Dec[idx] = uint16(r)
			}
		}
	}
}

// decodeCP932 は CP932 を UTF-8 文字列へ復号する。strict のときは不正バイトで
// ok=false を返す。それ以外は不正バイトを1バイトずつ U+FFFD に置き換える。
func decodeCP932(b []byte, strict bool) (string, bool) {
	ascii := true
	for _, c := range b {
		if c >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return string(b), true
	}
	cp932DecOnce.Do(buildCP932Dec)

	out := make([]byte, 0, len(b)*3/2)
	for i := 0; i < len(b); {
		c := int(b[i])
		var r rune
		size := 1
		switch {
		case c <= 0x80:
			r = rune(c)
		case c == 0xA0:
			r = 0xF8F0
		case c >= 0xA1 && c <= 0xDF:
			r = rune(0xFEC0 + c)
		case c >= 0xFD:
			r = rune(0xF8F1 - 0xFD + c)
		default:
			r = utf8.RuneError
			if i+1 < len(b) {
				if v := cp932Dec[(c-cp932LeadMin)<<8|int(b[i+1])]; v != 0 {
					r, size = rune(v), 2
				}
			}
			if size == 1 && strict {
				return "", false
			}
		}
		out = utf8.AppendRune(out, r)
		i += size
	}
	return string(out), true
}

// DecodeCP932 は不正バイトを U+FFFD に置き換えて復号する。
func DecodeCP932(b []byte) string {
	s, _ := decodeCP932(b, false)
	return s
}

func buildCP932Enc() {
	cp932DecOnce.Do(buildCP932Dec)
	cp932Enc = make([]uint16, 0x10000)
	enc := japanese.ShiftJIS.NewEncoder()
	var buf [8]byte
	for idx, v := range cp932Dec {
		if v == 0 || cp932Enc[v] != 0 {
			continue
		}
		// 重複割当のある文字 (NEC/IBM拡張) は x/text の優先順位に従う
		nDst, _, err := enc.Transform(buf[:], []byte(string(rune(v))), true)
		enc.Reset()
		if err == nil && nDst == 2 {
			cp932Enc[v] = uint16(buf[0])<<8 | uint16(buf[1])
		} else {
			cp932Enc[v] = uint16(idx + cp932LeadMin<<8)
		}
	}
	for r, code := range cp932EncExtra {
		cp932Enc[r] = code
	}
	for c := 0x80; c <= 0xFF; c++ {
		switch {
		case c == 0x80:
			cp932Enc[0x80] = 0x80
		case c == 0xA0:
			cp932Enc[0xF8F0] = 0xA0
		case c >= 0xA1 && c <= 0xDF:
			cp932Enc[0xFEC0+c] = uint16(c)
		case c >= 0xFD:
			cp932Enc[0xF8F1-0xFD+c] = uint16(c)
		}
	}
}

// AppendCP932 は s を CP932 で dst に追記する。表現できない文字は '?' にする。
func AppendCP932(dst []byte, s string) []byte {
	if isASCII(s) {
		return append(dst, s...)
	}
	cp932EncOnce.Do(buildCP932Enc)
	for _, r := range s {
		switch {
		case r < 0x80:
			dst = append(dst, byte(r))
		case r >= 0x10000 || cp932Enc[r] == 0:
			dst = append(dst, '?')
		case cp932Enc[r] < 0x100:
			dst = append(dst, byte(cp932Enc[r]))
		default:
			dst = append(dst, byte(cp932Enc[r]>>8), byte(cp932Enc[r]))
		}
	}
	return dst
}
