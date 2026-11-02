package engine

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

var layoutKeys = map[string]string{
	"name": "name", "名前": "name", "項目名": "name", "項目": "name",
	"start": "start", "開始": "start", "開始位置": "start", "位置": "start",
	"length": "length", "バイト数": "length", "長さ": "length", "len": "length",
	"type": "type", "形式": "type", "タイプ": "type", "型": "type",
	"scale": "scale", "小数桁": "scale", "小数": "scale",
}

// parseCSV は行分割済みテキストを CSV として解釈する。
// encoding/csv は厳格すぎる (引用符の途中出現でエラーになる) ため自前で持つ。
func parseCSV(lines []string) [][]string {
	const (
		startField = iota
		inField
		inQuoted
		quoteInQuoted
	)
	var rows [][]string
	var row []string
	var fld []rune
	state := startField
	save := func() {
		row = append(row, string(fld))
		fld = fld[:0]
	}
	for _, line := range lines {
		if state != inQuoted {
			row, state = nil, startField
		}
		for _, c := range line {
			switch state {
			case startField:
				switch c {
				case '"':
					state = inQuoted
				case ',':
					save()
				default:
					fld = append(fld, c)
					state = inField
				}
			case inField:
				if c == ',' {
					save()
					state = startField
				} else {
					fld = append(fld, c)
				}
			case inQuoted:
				if c == '"' {
					state = quoteInQuoted
				} else {
					fld = append(fld, c)
				}
			case quoteInQuoted:
				switch c {
				case '"':
					fld = append(fld, c)
					state = inQuoted
				case ',':
					save()
					state = startField
				default:
					fld = append(fld, c)
					state = inField
				}
			}
		}
		if state == inQuoted {
			continue // 引用符内の改行: 次の行へ続ける
		}
		if line != "" {
			save()
		}
		rows = append(rows, row)
	}
	if state == inQuoted {
		save()
		rows = append(rows, row)
	}
	return rows
}

func hasContent(row []string) bool {
	for _, x := range row {
		if strip(x) != "" {
			return true
		}
	}
	return false
}

// LoadLayoutCSV はレイアウトCSV (名前,開始,バイト数,形式,小数桁) を読み込む。
func LoadLayoutCSV(path string) ([]Field, error) {
	text, err := readTextAny(path, "")
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, r := range parseCSV(splitLines(text)) {
		if hasContent(r) {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("レイアウトファイルが空です: %s", path)
	}

	idx := map[string]int{}
	hasHeader := false
	for i, c := range rows[0] {
		h := strip(c)
		if isASCII(h) {
			h = strings.ToLower(h)
		}
		if key, ok := layoutKeys[h]; ok {
			idx[key] = i
			hasHeader = true
		}
	}
	dataRows := rows
	firstLine := 1
	if hasHeader {
		dataRows = rows[1:]
		firstLine = 2
	} else {
		idx = map[string]int{"name": 0, "start": 1, "length": 2, "type": 3, "scale": 4}
	}

	for _, key := range []string{"start", "length", "type"} {
		if _, ok := idx[key]; !ok {
			return nil, fmt.Errorf("レイアウトに列がありません: %s (開始/バイト数/形式)", key)
		}
	}

	column := func(r []string, key string) (string, bool) {
		i, ok := idx[key]
		if !ok || i >= len(r) {
			return "", false
		}
		return r[i], true
	}
	var fields []Field
	for i, r := range dataRows {
		ln := firstLine + i
		bad := func(reason any) error {
			return fmt.Errorf("レイアウト %s の %d 行目を解釈できません: %q (%v)", path, ln, r, reason)
		}
		name, _ := column(r, "name")
		name = strip(name)
		startText, ok1 := column(r, "start")
		lengthText, ok2 := column(r, "length")
		typeText, ok3 := column(r, "type")
		if !ok1 || !ok2 || !ok3 {
			return nil, bad("列が不足しています")
		}
		start, err := atoi(startText)
		if err != nil {
			return nil, bad(err)
		}
		length, err := atoi(lengthText)
		if err != nil {
			return nil, bad(err)
		}
		ftype, signed, err := NormalizeType(typeText)
		if err != nil {
			return nil, bad(err)
		}
		scale := 0
		if s, ok := column(r, "scale"); ok && strip(s) != "" {
			if scale, err = atoi(s); err != nil {
				return nil, bad(err)
			}
		}
		if start < 1 {
			return nil, fmt.Errorf("開始位置は1始まりで指定してください (%d行目: %d)", ln, start)
		}
		if length < 0 || scale < 0 {
			return nil, bad("バイト数と小数桁は0以上で指定してください")
		}
		if name == "" {
			name = fmt.Sprintf("F%03d", ln)
		}
		fields = append(fields, NewField(name, start-1, length, ftype, scale, signed))
	}
	return fields, nil
}

// ParseFieldArgs は --field "名前,開始,バイト数,形式[,小数桁]" を解釈する。
func ParseFieldArgs(specs []string) ([]Field, error) {
	var fields []Field
	for i, spec := range specs {
		parts := strings.Split(spec, ",")
		for j := range parts {
			parts[j] = strip(parts[j])
		}
		if len(parts) < 4 {
			return nil, fmt.Errorf("--field は 名前,開始,バイト数,形式[,小数桁] で指定してください: %q", spec)
		}
		start, err := atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("--field の開始位置が不正です: %q", spec)
		}
		length, err := atoi(parts[2])
		if err != nil {
			return nil, fmt.Errorf("--field のバイト数が不正です: %q", spec)
		}
		scale := 0
		if len(parts) > 4 && parts[4] != "" {
			if scale, err = atoi(parts[4]); err != nil {
				return nil, fmt.Errorf("--field の小数桁が不正です: %q", spec)
			}
		}
		if start < 1 || length < 0 || scale < 0 {
			return nil, fmt.Errorf("--field の開始位置は1以上、バイト数と小数桁は0以上で指定してください: %q", spec)
		}
		ftype, signed, err := NormalizeType(parts[3])
		if err != nil {
			return nil, err
		}
		name := parts[0]
		if name == "" {
			name = fmt.Sprintf("F%03d", i+1)
		}
		fields = append(fields, NewField(name, start-1, length, ftype, scale, signed))
	}
	return fields, nil
}

// SaveLayoutCSV はレイアウトを BOM 付き UTF-8 の CSV で保存する。
func SaveLayoutCSV(fields []Field, path string) error {
	buf := []byte(utf8BOM)
	buf = appendCSVRow(buf, []string{"名前", "開始", "バイト数", "形式", "小数桁"})
	for _, f := range fields {
		scale := ""
		if f.Scale != 0 {
			scale = strconv.Itoa(f.Scale)
		}
		buf = appendCSVRow(buf, []string{f.Name, strconv.Itoa(f.Start + 1),
			strconv.Itoa(f.Length), f.Type, scale})
	}
	return os.WriteFile(path, buf, 0o666)
}

// FormatLayout はレイアウトの一覧表を文字列にする。
func FormatLayout(fields []Field, recordLength int) string {
	typeNames := map[string]string{"X": "半角文字", "K": "半角カナ", "N": "全角文字",
		"9": "ゾーン数値", "P": "パック", "B": "バイナリ"}
	rule := strings.Repeat("-", 60)
	lines := []string{fmt.Sprintf("%-20s %5s %5s %-4s %s", "名前", "開始", "バイト", "形式", "内容"), rule}
	for _, f := range fields {
		desc, ok := typeNames[f.Type]
		if !ok {
			desc = f.Type
		}
		if f.Signed {
			desc += "(符号付)"
		}
		if f.Scale != 0 {
			desc += fmt.Sprintf(" 小数%d桁", f.Scale)
		}
		if f.Type == "P" || f.Type == "B" {
			desc += fmt.Sprintf(" %d桁", f.Digits)
		}
		lines = append(lines, fmt.Sprintf("%-20s %5d %5d %-4s %s", f.Name, f.Start+1, f.Length, f.Type, desc))
	}
	lines = append(lines, rule, fmt.Sprintf("レコード長: %d バイト", recordLength))
	return strings.Join(lines, "\n")
}
