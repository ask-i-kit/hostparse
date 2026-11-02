package engine

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// isSpace は Python の str.isspace と同じ判定を行う。
// unicode.IsSpace は 0x1C-0x1F を含まないため自前で持つ。
func isSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0D, r >= 0x1C && r <= 0x20, r == 0x85, r == 0xA0:
		return true
	case r < 0x1680:
		return false
	case r == 0x1680, r >= 0x2000 && r <= 0x200A, r == 0x2028, r == 0x2029,
		r == 0x202F, r == 0x205F, r == 0x3000:
		return true
	}
	return false
}

func strip(s string) string  { return strings.TrimFunc(s, isSpace) }
func rstrip(s string) string { return strings.TrimRightFunc(s, isSpace) }
func lstrip(s string) string { return strings.TrimLeftFunc(s, isSpace) }

// isDigitRune は CP932 で表現できる範囲について str.isdigit と同じ判定を行う。
// 全角数字のほか ①〜⑨ も数字として扱われる。
func isDigitRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 0xFF10 && r <= 0xFF19 || r >= 0x2460 && r <= 0x2468
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isDigitRune(r) {
			return false
		}
	}
	return true
}

func isASCIIDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// splitLines は str.splitlines と同じ区切りで行に分解する。
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, r := range s {
		switch r {
		case '\n', '\v', '\f', 0x1C, 0x1D, 0x1E, 0x85, 0x2028, 0x2029:
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				continue // \r\n は \n 側で区切る
			}
		default:
			continue
		}
		lines = append(lines, s[start:i])
		start = i + utf8.RuneLen(r)
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return trimCR(lines)
}

// trimCR は \r\n 区切りの行末に残った \r を取り除く。
func trimCR(lines []string) []string {
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func atoi(s string) (int, error) {
	return strconv.Atoi(strip(s))
}

// Comma は 1234567 -> 1,234,567 のように3桁区切りにする。
func Comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
