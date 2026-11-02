package engine

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	utf8BOM   = "\xEF\xBB\xBF"
	chunkSize = 8 * 1024 * 1024
)

// ErrorCounter は変換エラーの件数と最初の1件を記録する。
type ErrorCounter struct {
	Count      int64
	FirstName  string
	FirstValue string
}

func (e *ErrorCounter) hit(name, value string) {
	e.Count++
	if e.Count == 1 {
		e.FirstName, e.FirstValue = name, value
	}
}

// ゾーン数値の符号オーバーパンチ (EBCDIC由来 / ASCII系)
func overpunch(r rune) (digit rune, neg, ok bool) {
	switch {
	case r == '{':
		return '0', false, true
	case r >= 'A' && r <= 'I':
		return r - 'A' + '1', false, true
	case r == '}':
		return '0', true, true
	case r >= 'J' && r <= 'R':
		return r - 'J' + '1', true, true
	}
	return 0, false, false
}

func applyScale(digits string, scale int, keepZeros bool) string {
	s := digits
	if scale != 0 {
		r := []rune(digits)
		if len(r) <= scale {
			r = append([]rune(strings.Repeat("0", scale+1-len(r))), r...)
		}
		cut := len(r) - scale
		s = string(r[:cut]) + "." + string(r[cut:])
	}
	if !keepZeros {
		s = strings.TrimLeft(s, "0")
		if s == "" {
			s = "0"
		}
		if s[0] == '.' {
			s = "0" + s
		}
	}
	return s
}

func withSign(v string, neg bool) string {
	if neg && strings.Trim(v, "0.") != "" {
		return "-" + v
	}
	return v
}

// NewConverter は項目のバイト列を文字列へ変換する関数を返す。
func NewConverter(f Field, stripText, keepZeros bool, errs *ErrorCounter) func([]byte) string {
	name, scale, signed := f.Name, f.Scale, f.Signed

	switch f.Type {
	case "9":
		return func(b []byte) string {
			// ファストパス: 純粋な数字のみ (大半のケース)
			if isASCIIDigits(b) {
				if scale != 0 || !keepZeros {
					return applyScale(string(b), scale, keepZeros)
				}
				return string(b)
			}
			s := []rune(strip(DecodeCP932(b)))
			if len(s) == 0 {
				return ""
			}
			neg := false
			last := s[len(s)-1]
			if s[0] == '+' || s[0] == '-' {
				neg = s[0] == '-'
				s = s[1:]
			} else if last == '+' || last == '-' {
				neg = last == '-'
				s = s[:len(s)-1]
			} else if d, n, ok := overpunch(last); ok {
				s[len(s)-1] = d
				neg = n
			}
			if !isDigits(string(s)) {
				h := hex.EncodeToString(b)
				errs.hit(name, h)
				return "#ERR:" + h
			}
			return withSign(applyScale(string(s), scale, keepZeros), neg)
		}

	case "P":
		return func(b []byte) string {
			if len(b) == 0 {
				return ""
			}
			h := hex.EncodeToString(b)
			digits, sign := h[:len(h)-1], h[len(h)-1]
			neg := sign == 'd' || sign == 'b'
			if !isDigits(digits) || !(neg || sign == 'c' || sign == 'f' || sign == 'a' || sign == 'e') {
				if len(strings.Trim(string(b), "\x00 ")) == 0 {
					return "" // 全ゼロ埋め/空白は空とみなす
				}
				errs.hit(name, h)
				return "#ERR:" + h
			}
			return withSign(applyScale(digits, scale, keepZeros), neg)
		}

	case "B":
		return func(b []byte) string {
			s, neg := binaryText(b, signed)
			if scale != 0 {
				if len(s) < scale+1 {
					s = strings.Repeat("0", scale+1-len(s)) + s
				}
				s = applyScale(s, scale, keepZeros)
			}
			if neg {
				return "-" + s
			}
			return s
		}
	}

	if stripText {
		return func(b []byte) string { return rstrip(DecodeCP932(b)) }
	}
	return DecodeCP932
}

// binaryText はビッグエンディアンの整数を絶対値の文字列と負号に分けて返す。
func binaryText(b []byte, signed bool) (string, bool) {
	neg := signed && len(b) > 0 && b[0]&0x80 != 0
	if len(b) <= 8 {
		var u uint64
		for _, c := range b {
			u = u<<8 | uint64(c)
		}
		if neg {
			u = -u
			if len(b) < 8 {
				u &= 1<<(8*uint(len(b))) - 1
			}
		}
		return strconv.FormatUint(u, 10), neg
	}
	n := new(big.Int).SetBytes(b)
	if neg {
		n.Sub(new(big.Int).Lsh(big.NewInt(1), uint(8*len(b))), n)
	}
	return n.String(), neg
}

// appendCSVRow は1行分の CSV を CRLF 付きで追記する。
// 区切り・引用符・改行を含むときだけ引用する (encoding/csv は先頭空白も引用してしまう)。
func appendCSVRow(dst []byte, row []string) []byte {
	if len(row) == 1 && row[0] == "" {
		return append(dst, `""`+"\r\n"...)
	}
	for i, v := range row {
		if i > 0 {
			dst = append(dst, ',')
		}
		if strings.ContainsAny(v, ",\"\r\n") {
			dst = append(dst, '"')
			dst = append(dst, strings.ReplaceAll(v, `"`, `""`)...)
			dst = append(dst, '"')
		} else {
			dst = append(dst, v...)
		}
	}
	return append(dst, "\r\n"...)
}

func padRight(s string, w int) string {
	if n := w - utf8.RuneCountInString(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func padLeft(s string, w int) string {
	if n := w - utf8.RuneCountInString(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

// appendTextLine は text 形式の1行を CRLF 付きで追記する。
func appendTextLine(dst []byte, line string) []byte {
	if strings.IndexByte(line, '\n') >= 0 {
		line = strings.ReplaceAll(line, "\n", "\r\n") // Windows のテキスト出力と同じ改行
	}
	dst = append(dst, line...)
	return append(dst, "\r\n"...)
}

// SamePath は2つのパスが同じファイルを指すかどうかを返す。
func SamePath(a, b string) bool {
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(sa, sb)
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && strings.EqualFold(absA, absB)
}

// ConvertOptions は Convert の入力。
type ConvertOptions struct {
	Input        string
	Output       string    // 空なら Stdout へ書き出す
	Stdout       io.Writer // Output が空のときの出力先
	Fields       []Field
	RecordLength int
	CSV          bool   // false なら text 形式
	ANSI         bool   // true なら CP932, false なら UTF-8 (CSV は BOM 付き)
	Delimiter    string // text 形式の項目区切り
	Header       bool   // 見出し行を出力する
	KeepZeros    bool
	Limit        int64 // 0 なら無制限
	Skip         int64
	Progress     func(fraction float64)
}

// ConvertResult は Convert の結果。
type ConvertResult struct {
	Records int64
	Errors  ErrorCounter
}

// Convert は入力ファイル全体を変換して出力する。チャンク単位で読むため
// ファイルサイズによらずメモリ使用量は一定。
func Convert(o ConvertOptions) (res ConvertResult, err error) {
	if o.RecordLength <= 0 {
		return res, errors.New("レコード長が不正です")
	}
	if o.Output != "" && SamePath(o.Input, o.Output) {
		return res, errors.New("出力先が入力ファイルと同じです")
	}
	in, err := os.Open(o.Input)
	if err != nil {
		return res, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return res, err
	}

	out := o.Stdout
	if o.Output != "" {
		fp, cerr := os.Create(o.Output)
		if cerr != nil {
			return res, cerr
		}
		defer func() {
			if cerr := fp.Close(); err == nil {
				err = cerr
			}
		}()
		out = fp
	}
	w := bufio.NewWriterSize(out, 1<<20)
	defer func() {
		if ferr := w.Flush(); err == nil {
			err = ferr
		}
	}()

	if o.CSV && !o.ANSI {
		if _, err = w.WriteString(utf8BOM); err != nil {
			return res, err
		}
	}

	convs := make([]func([]byte) string, len(o.Fields))
	widths := make([]int, len(o.Fields))
	for i, f := range o.Fields {
		convs[i] = NewConverter(f, o.CSV, o.KeepZeros, &res.Errors)
		widths[i] = DisplayWidth(f)
	}

	var line, encoded []byte
	row := make([]string, len(o.Fields))
	emit := func() error {
		data := line
		if o.ANSI {
			encoded = AppendCP932(encoded[:0], string(line))
			data = encoded
		}
		_, err := w.Write(data)
		return err
	}
	formatText := func(numericPad bool) []byte {
		var b strings.Builder
		for i, v := range row {
			if i > 0 {
				b.WriteString(o.Delimiter)
			}
			if numericPad && o.Fields[i].IsNumeric() {
				b.WriteString(padLeft(v, widths[i]))
			} else {
				b.WriteString(padRight(v, widths[i]))
			}
		}
		return appendTextLine(line[:0], b.String())
	}

	if o.Header {
		for i, f := range o.Fields {
			row[i] = f.Name
		}
		if o.CSV {
			line = appendCSVRow(line[:0], row)
		} else {
			line = formatText(false)
		}
		if err = emit(); err != nil {
			return res, err
		}
	}

	rl := int64(o.RecordLength)
	if o.Skip != 0 {
		if _, err = in.Seek(o.Skip*rl, io.SeekStart); err != nil {
			return res, err
		}
	}
	chunk := make([]byte, max(1, chunkSize/rl)*rl)
	var done int64
	limit := o.Limit
	for limit == 0 || res.Records < limit {
		n, rerr := io.ReadFull(in, chunk)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			return res, rerr
		}
		if int64(n) < rl {
			break
		}
		usable := int64(n) / rl * rl
		for off := int64(0); off < usable; off += rl {
			rec := chunk[off : off+rl]
			for i, f := range o.Fields {
				row[i] = convs[i](rec[f.Start:f.End()])
			}
			if o.CSV {
				line = appendCSVRow(line[:0], row)
			} else {
				line = formatText(true)
			}
			if err = emit(); err != nil {
				return res, err
			}
			res.Records++
			if limit != 0 && res.Records >= limit {
				break
			}
		}
		done += int64(n)
		if o.Progress != nil && st.Size() > 0 {
			o.Progress(float64(done) / float64(st.Size()))
		}
	}
	return res, nil
}

// ReadHead はファイルの先頭最大 n バイトを読む。
func ReadHead(path string, n int) ([]byte, error) {
	fp, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fp.Close()
	buf := make([]byte, n)
	got, err := io.ReadFull(fp, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:got], nil
}

// SampleRecords は先頭から最大 n レコードを読み、レコード単位に分けて返す。
func SampleRecords(path string, recordLength, n int) ([][]byte, error) {
	data, err := ReadHead(path, recordLength*n)
	if err != nil {
		return nil, err
	}
	records := make([][]byte, 0, len(data)/recordLength)
	for off := 0; off+recordLength <= len(data); off += recordLength {
		records = append(records, data[off:off+recordLength])
	}
	return records, nil
}

// PreviewRows は先頭 n 件だけを読んで行データを組み立てる。
// 読込量はレコード長 × 件数で頭打ちなので、ファイルサイズには依存しない。
func PreviewRows(path string, recordLength int, fields []Field, keepZeros bool, n int) ([][]string, int64, error) {
	records, err := SampleRecords(path, recordLength, n)
	if err != nil {
		return nil, 0, err
	}
	var errs ErrorCounter
	convs := make([]func([]byte) string, len(fields))
	for i, f := range fields {
		convs[i] = NewConverter(f, true, keepZeros, &errs)
	}
	rows := make([][]string, len(records))
	for r, rec := range records {
		row := make([]string, len(fields))
		for i, f := range fields {
			row[i] = convs[i](rec[f.Start:f.End()])
		}
		rows[r] = row
	}
	return rows, errs.Count, nil
}

// HexDump は 1行16バイトのダンプを返す。recordLength > 0 ならレコード単位で区切る。
func HexDump(data []byte, recordLength, records int) string {
	hexLines := func(seq []byte) []string {
		var out []string
		for off := 0; off < len(seq); off += 16 {
			seg := seq[off:min(off+16, len(seq))]
			var hexs strings.Builder
			for i, b := range seg {
				if i > 0 {
					hexs.WriteByte(' ')
				}
				fmt.Fprintf(&hexs, "%02X", b)
			}
			out = append(out, fmt.Sprintf("%5d: %-47s | %s", off+1, hexs.String(), Printable(DecodeCP932(seg))))
		}
		return out
	}
	if recordLength <= 0 {
		return strings.Join(hexLines(data), "\n")
	}
	var lines []string
	for r := 0; r < records && (r+1)*recordLength <= len(data); r++ {
		lines = append(lines, fmt.Sprintf("===== レコード %d =====", r+1))
		lines = append(lines, hexLines(data[r*recordLength:(r+1)*recordLength])...)
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// Printable は画面表示できない文字を ･ に置き換える。
// NUL は Win32 コントロールで文字列の終端になるため、そのままでは表示できない。
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7F && r < 0xA0 || r == utf8.RuneError {
			return '･'
		}
		return r
	}, s)
}
