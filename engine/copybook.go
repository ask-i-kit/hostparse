package engine

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// CopyItem はコピー句 (COBOL COPY句) の1データ項目。
type CopyItem struct {
	Level     int
	Name      string
	Pic       string
	Usage     string // DISPLAY / PACK / BINARY
	Occurs    int
	Redefines string
	Children  []*CopyItem
}

// readTextAny はテキストファイルを読み、文字コードを判定して復号する。
// preferred → UTF-8 → CP932 の順に試し、全滅なら CP932 で置換復号する。
func readTextAny(path, preferred string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if preferred != "" {
		s, ok, err := decodeNamed(raw, preferred)
		if err != nil {
			return "", err
		}
		if ok {
			return s, nil
		}
	}
	if utf8.Valid(raw) {
		return string(bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF"))), nil
	}
	if s, ok := decodeCP932(raw, true); ok {
		return s, nil
	}
	return DecodeCP932(raw), nil
}

// decodeNamed は名前で指定された文字コードで復号する。
func decodeNamed(raw []byte, name string) (string, bool, error) {
	key := strings.ReplaceAll(strings.ToLower(strip(name)), "_", "-")
	switch key {
	case "utf-8", "utf8":
		return string(raw), utf8.Valid(raw), nil
	case "utf-8-sig", "utf8-sig":
		return string(bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF"))), utf8.Valid(raw), nil
	case "cp932", "ms932", "windows-31j", "mskanji", "ms-kanji", "shift-jis", "shiftjis", "sjis", "s-jis":
		s, ok := decodeCP932(raw, true)
		return s, ok, nil
	}
	enc, err := htmlindex.Get(key)
	if err != nil {
		return "", false, fmt.Errorf("未対応の文字コードです: %s", name)
	}
	out, err := enc.NewDecoder().Bytes(raw)
	if err != nil || bytes.ContainsRune(out, utf8.RuneError) {
		return "", false, nil
	}
	return string(out), true, nil
}

// copybookStatements はコメント・行番号領域を除去し、ピリオド区切りの文へ分解する。
func copybookStatements(text string) []string {
	var bodies []string
	for _, raw := range splitLines(text) {
		trimmed := rstrip(raw)
		if strip(trimmed) == "" {
			continue
		}
		line := []rune(trimmed)
		// 固定形式 (1-6桁: 行番号, 7桁目: 標識, 73桁目以降: 識別領域) の判定
		head := string(line[:min(6, len(line))])
		if hs := strip(head); len(line) > 6 && (hs == "" || isDigits(hs)) {
			if line[6] == '*' || line[6] == '/' {
				continue
			}
			bodies = append(bodies, string(line[7:min(72, len(line))]))
		} else {
			if strings.HasPrefix(lstrip(trimmed), "*") {
				continue
			}
			bodies = append(bodies, trimmed)
		}
	}
	src := []rune(strings.Join(bodies, " "))

	// 引用符内を保護しつつピリオドで分割
	var stmts []string
	var buf []rune
	var quote rune
	flush := func() {
		if s := strip(string(buf)); s != "" {
			stmts = append(stmts, s)
		}
		buf = buf[:0]
	}
	for i, ch := range src {
		switch {
		case quote != 0:
			buf = append(buf, ch)
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
			buf = append(buf, ch)
		case ch == '.' && (i+1 >= len(src) || src[i+1] == ' ' || src[i+1] == '\t'):
			flush()
		default:
			buf = append(buf, ch)
		}
	}
	flush()
	return stmts
}

// RE2 の \w \s \b は ASCII 限定なので、日本語の項目名を扱えるよう明示的に定義する。
const (
	reWord     = `\p{L}\p{N}_`
	reSpace    = `[\t-\r\x{1C}-\x{1F}\x{85}\p{Z}]`
	reBoundary = `(?:^|[^` + reWord + `])`
	reEndBound = `(?:$|[^` + reWord + `])`
)

var (
	picRe = regexp.MustCompile(`(?i)` + reBoundary + `PIC(?:TURE)?(?:` + reSpace + `+IS)?` + reSpace +
		`+([` + reWord + `()VvSsXxNnZz9.,*+\-/$]+)`)
	usageRe = regexp.MustCompile(`(?i)` + reBoundary + `(COMP-3|COMPUTATIONAL-3|PACKED-DECIMAL|COMP-5|COMPUTATIONAL-5|COMP-4|` +
		`COMPUTATIONAL-4|COMP-1|COMP-2|COMP|COMPUTATIONAL|BINARY|DISPLAY)` + reEndBound)
	occursRe = regexp.MustCompile(`(?i)` + reBoundary + `OCCURS` + reSpace + `+(?:[0-9]+` + reSpace + `+TO` + reSpace + `+)?([0-9]+)`)
	redefRe  = regexp.MustCompile(`(?i)` + reBoundary + `REDEFINES` + reSpace + `+([` + reWord + `\-]+)`)
	stmtRe   = regexp.MustCompile(`(?s)^([0-9]{1,2})` + reSpace + `+([` + reWord + `\-]+)?` + reSpace + `*(.*)$`)
	expandRe = regexp.MustCompile(`([XANZ9*BP])\(([0-9]+)\)`)
)

// ParseCopybook はコピー句を解析して CopyItem のツリー(トップレベルのリスト)を返す。
func ParseCopybook(path, encoding string) ([]*CopyItem, error) {
	text, err := readTextAny(path, encoding)
	if err != nil {
		return nil, err
	}
	var items []*CopyItem
	for _, stmt := range copybookStatements(text) {
		m := stmtRe.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		level, _ := strconv.Atoi(m[1])
		if level == 66 || level == 88 {
			continue // 77 は独立項目として扱う
		}
		name := strings.ToUpper(m[2])
		if name == "" {
			name = "FILLER"
		}
		rest := m[3]
		switch name {
		case "PIC", "PICTURE", "REDEFINES", "OCCURS", "VALUE":
			rest = name + " " + rest
			name = "FILLER"
		}
		item := &CopyItem{Level: level, Name: name, Usage: "DISPLAY"}
		if pm := picRe.FindStringSubmatch(rest); pm != nil {
			item.Pic = strings.ToUpper(pm[1])
		}
		if um := usageRe.FindStringSubmatch(rest); um != nil {
			switch strings.ToUpper(um[1]) {
			case "COMP-3", "COMPUTATIONAL-3", "PACKED-DECIMAL":
				item.Usage = "PACK"
			case "COMP", "COMPUTATIONAL", "COMP-4", "COMPUTATIONAL-4",
				"COMP-5", "COMPUTATIONAL-5", "BINARY":
				item.Usage = "BINARY"
			}
		}
		if om := occursRe.FindStringSubmatch(rest); om != nil {
			if item.Occurs, err = strconv.Atoi(om[1]); err != nil {
				return nil, fmt.Errorf("OCCURS の回数が不正です: %s", om[1])
			}
		}
		if rm := redefRe.FindStringSubmatch(rest); rm != nil {
			item.Redefines = strings.ToUpper(rm[1])
		}
		items = append(items, item)
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("コピー句からデータ項目を読み取れませんでした: %s", path)
	}

	// レベル番号でツリー構築
	root := &CopyItem{Name: "__ROOT__"}
	stack := []*CopyItem{root}
	for _, it := range items {
		for len(stack) > 1 && stack[len(stack)-1].Level >= it.Level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, it)
		stack = append(stack, it)
	}
	return root.Children, nil
}

// expandPic は X(3)9(2) -> XXX99 のように展開する。
func expandPic(pic string) string {
	return expandRe.ReplaceAllStringFunc(pic, func(m string) string {
		sub := expandRe.FindStringSubmatch(m)
		n, err := strconv.Atoi(sub[2])
		if err != nil {
			return m
		}
		return strings.Repeat(sub[1], n)
	})
}

func countAny(s, chars string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(chars, s[i]) >= 0 {
			n++
		}
	}
	return n
}

// picInfo は PIC文字列から (バイト数, ftype, digits, scale, signed) を求める。
func picInfo(pic, usage string) (size int, ftype string, digits, scale int, signed bool) {
	p := expandPic(pic)
	signed = strings.HasPrefix(p, "S")
	if signed {
		p = p[1:]
	}
	if strings.Contains(p, "N") {
		return strings.Count(p, "N") * 2, "N", 0, 0, false
	}
	if strings.ContainsAny(p, "XA") {
		return countAny(p, "XA9"), "X", 0, 0, false
	}
	// 数値
	_, decpart, _ := strings.Cut(p, "V")
	digits = countAny(p, "9Z*")
	scale = countAny(decpart, "9Z*")
	editChars := countAny(p, ".,+-/$B0")
	switch usage {
	case "PACK":
		return digits/2 + 1, "P", digits, scale, signed
	case "BINARY":
		size = 8
		if digits <= 4 {
			size = 2
		} else if digits <= 9 {
			size = 4
		}
		return size, "B", digits, scale, signed
	}
	// DISPLAY (ゾーン数値 / 編集項目)
	size = digits + editChars
	if editChars != 0 {
		return size, "X", 0, 0, false // 編集項目は文字として扱う
	}
	return size, "9", digits, scale, signed
}

// CopybookToFields はコピー句ツリーを平坦化し、Field のリストと合計バイト数を返す。
func CopybookToFields(nodes []*CopyItem) ([]Field, int) {
	var fields []Field

	var walk func(children []*CopyItem, start int, suffix string) int
	walk = func(children []*CopyItem, start int, suffix string) int {
		starts := map[string]int{}
		cur, maxend := start, start
		for _, c := range children {
			pos, advance := cur, true
			if c.Redefines != "" {
				if p, ok := starts[c.Redefines]; ok {
					pos, advance = p, false
				}
			}
			starts[c.Name] = pos
			n := c.Occurs
			if n == 0 {
				n = 1
			}
			p := pos
			for i := 0; i < n; i++ {
				sfx := suffix
				if c.Occurs != 0 {
					sfx += fmt.Sprintf("(%d)", i+1)
				}
				var end int
				switch {
				case len(c.Children) > 0:
					end = walk(c.Children, p, sfx)
				case c.Pic != "":
					size, ftype, digits, scale, signed := picInfo(c.Pic, c.Usage)
					if c.Name != "FILLER" {
						if digits == 0 {
							digits = size
						}
						f := Field{Name: c.Name + sfx, Start: p, Length: size, Type: ftype,
							Scale: scale, Signed: signed, Digits: digits}
						f.fillDigits()
						fields = append(fields, f)
					}
					end = p + size
				default:
					end = p // PIC なしの単独項目 (サイズ0)
				}
				p = end
			}
			if advance {
				cur = p
			}
			maxend = max(maxend, p)
		}
		return maxend
	}

	total := walk(nodes, 0, "")
	return fields, total
}
