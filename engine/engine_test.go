package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func convert(t *testing.T, f Field, strip, keepZeros bool, b []byte) (string, int64) {
	t.Helper()
	var errs ErrorCounter
	return NewConverter(f, strip, keepZeros, &errs)(b), errs.Count
}

func TestZoned(t *testing.T) {
	tests := []struct {
		in        string
		scale     int
		keepZeros bool
		want      string
		errs      int64
	}{
		{"00123", 0, false, "123", 0},
		{"00123", 0, true, "00123", 0},
		{"00000", 0, false, "0", 0},
		{"00123", 2, false, "1.23", 0},
		{"00003", 2, false, "0.03", 0},
		{"00003", 2, true, "000.03", 0},
		{"12", 4, false, "0.0012", 0},
		{"-0123", 0, false, "-123", 0},
		{"+0123", 0, false, "123", 0},
		{"0123-", 0, false, "-123", 0},
		{"-0000", 2, false, "0.00", 0}, // 負のゼロに符号は付けない
		{"0012C", 0, false, "123", 0},  // オーバーパンチ 正
		{"0012L", 0, false, "-123", 0}, // オーバーパンチ 負
		{"0012{", 0, false, "120", 0},
		{"0012}", 0, false, "-120", 0},
		{"     ", 0, false, "", 0},
		{"  123", 0, false, "123", 0},
		{"12X34", 0, false, "#ERR:3132583334", 1},
		{"+", 0, false, "#ERR:2b", 1},
	}
	for _, tt := range tests {
		f := NewField("Z", 0, len(tt.in), "9", tt.scale, false)
		got, errs := convert(t, f, true, tt.keepZeros, []byte(tt.in))
		if got != tt.want || errs != tt.errs {
			t.Errorf("%q scale=%d: got %q (errs=%d), want %q (errs=%d)", tt.in, tt.scale, got, errs, tt.want, tt.errs)
		}
	}
}

func TestPacked(t *testing.T) {
	tests := []struct {
		in    []byte
		scale int
		want  string
		errs  int64
	}{
		{[]byte{0x12, 0x34, 0x5C}, 0, "12345", 0},
		{[]byte{0x12, 0x34, 0x5D}, 0, "-12345", 0},
		{[]byte{0x12, 0x34, 0x5F}, 2, "123.45", 0},
		{[]byte{0x00, 0x00, 0x5B}, 2, "-0.05", 0},
		{[]byte{0x00, 0x00, 0x0D}, 2, "0.00", 0},
		{[]byte{0x00, 0x00, 0x00}, 0, "", 0}, // ゼロ埋めは空
		{[]byte{0x20, 0x20, 0x20}, 0, "", 0}, // 空白は空
		{[]byte{0x1A, 0x34, 0x5C}, 0, "#ERR:1a345c", 1},
		{[]byte{0x12, 0x34, 0x51}, 0, "#ERR:123451", 1},
	}
	for _, tt := range tests {
		f := NewField("P", 0, len(tt.in), "P", tt.scale, false)
		got, errs := convert(t, f, true, false, tt.in)
		if got != tt.want || errs != tt.errs {
			t.Errorf("%x: got %q (errs=%d), want %q (errs=%d)", tt.in, got, errs, tt.want, tt.errs)
		}
	}
}

func TestBinary(t *testing.T) {
	tests := []struct {
		in     []byte
		signed bool
		scale  int
		want   string
	}{
		{[]byte{0x01, 0x00}, false, 0, "256"},
		{[]byte{0xFF, 0xFF}, false, 0, "65535"},
		{[]byte{0xFF, 0xFF}, true, 0, "-1"},
		{[]byte{0x80, 0x00}, true, 0, "-32768"},
		{[]byte{0xFF, 0x85}, true, 2, "-1.23"},
		{[]byte{0x80, 0, 0, 0, 0, 0, 0, 0}, true, 0, "-9223372036854775808"},
		{[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, false, 0, "18446744073709551615"},
		{[]byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0}, false, 0, "18446744073709551616"},
		{[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE}, true, 0, "-2"},
	}
	for _, tt := range tests {
		f := NewField("B", 0, len(tt.in), "B", tt.scale, tt.signed)
		if got, _ := convert(t, f, true, false, tt.in); got != tt.want {
			t.Errorf("%x signed=%v: got %q, want %q", tt.in, tt.signed, got, tt.want)
		}
	}
}

func TestTextStrip(t *testing.T) {
	in := append(AppendCP932(nil, "田中　"), ' ', 0x1F)
	f := NewField("N", 0, len(in), "N", 0, false)
	if got, _ := convert(t, f, true, false, in); got != "田中" {
		t.Errorf("strip: got %q", got)
	}
	if got, _ := convert(t, f, false, false, in); got != "田中　 \x1F" {
		t.Errorf("no strip: got %q", got)
	}
}

func TestCP932(t *testing.T) {
	tests := []struct {
		in   []byte
		want string
	}{
		{[]byte("abc"), "abc"},
		{[]byte{0xB1, 0xDF}, "ｱﾟ"},
		{[]byte{0x93, 0xFA, 0x96, 0x7B}, "日本"},
		{[]byte{0xFA, 0x40}, "ⅰ"},  // IBM拡張
		{[]byte{0xF0, 0x40}, ""},  // 外字
		{[]byte{0x81}, "�"},        // 途中で切れた先行バイト
		{[]byte{0x81, 0x20}, "� "}, // 不正な後続バイトは残す
		{[]byte{0x80, 0xA0, 0xFD}, "\u0080"},
	}
	for _, tt := range tests {
		if got := DecodeCP932(tt.in); got != tt.want {
			t.Errorf("Decode %x: got %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := AppendCP932(nil, "日本ｱa☃"); !bytes.Equal(got, []byte{0x93, 0xFA, 0x96, 0x7B, 0xB1, 'a', '?', 0xF0, 0x40}) {
		t.Errorf("Append: got %x", got)
	}
}

func TestAppendCSVRow(t *testing.T) {
	tests := []struct {
		row  []string
		want string
	}{
		{[]string{"a", "b"}, "a,b\r\n"},
		{[]string{" a", "b "}, " a,b \r\n"}, // 空白だけでは引用しない
		{[]string{"a,b", `c"d`, "e\nf"}, "\"a,b\",\"c\"\"d\",\"e\nf\"\r\n"},
		{[]string{""}, "\"\"\r\n"},
		{[]string{"", ""}, ",\r\n"},
	}
	for _, tt := range tests {
		if got := string(appendCSVRow(nil, tt.row)); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.row, got, tt.want)
		}
	}
}

func TestParseCSV(t *testing.T) {
	got := parseCSV([]string{`a,"b,c",d"e`, ``, `"x""y",`, `"multi`, `line"`})
	want := [][]string{{"a", "b,c", `d"e`}, nil, {`x"y`, ""}, {"multiline"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines("a\r\nb\nc\rd\x0ce\n")
	if want := []string{"a", "b", "c", "d", "e"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCopybook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.cpy")
	src := "000100* コメント\n" +
		"       01  REC.\n" +
		"           05  社員番号  PIC 9(5).\n" +
		"           05  FILLER    PIC X(2).\n" +
		"           05  KIN       PIC S9(7)V99 COMP-3.\n" +
		"           05  CNT       PIC S9(4) COMP.\n" +
		"           05  MEISAI    OCCURS 2 TIMES.\n" +
		"               10  CD    PIC X(3).\n" +
		"           05  ALT REDEFINES MEISAI PIC X(6).\n" +
		"           05  EDIT      PIC ZZ,ZZ9.\n" +
		"           88  FLAG      VALUE 'A.B'.\n"
	if err := os.WriteFile(path, []byte(src), 0o666); err != nil {
		t.Fatal(err)
	}
	nodes, err := ParseCopybook(path, "")
	if err != nil {
		t.Fatal(err)
	}
	fields, total := CopybookToFields(nodes)
	want := []Field{
		{Name: "社員番号", Start: 0, Length: 5, Type: "9", Digits: 5},
		{Name: "KIN", Start: 7, Length: 5, Type: "P", Scale: 2, Signed: true, Digits: 9},
		{Name: "CNT", Start: 12, Length: 2, Type: "B", Signed: true, Digits: 4},
		{Name: "CD(1)", Start: 14, Length: 3, Type: "X", Digits: 3},
		{Name: "CD(2)", Start: 17, Length: 3, Type: "X", Digits: 3},
		{Name: "ALT", Start: 14, Length: 6, Type: "X", Digits: 6},
		{Name: "EDIT", Start: 20, Length: 6, Type: "X", Digits: 6},
	}
	if !reflect.DeepEqual(fields, want) {
		t.Errorf("fields:\n got %+v\nwant %+v", fields, want)
	}
	if total != 26 {
		t.Errorf("total = %d, want 26", total)
	}
}

func TestLayoutRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.csv")
	fields := []Field{
		NewField("A,B", 0, 5, "9", 0, false),
		NewField("給与", 5, 5, "P", 2, false),
	}
	if err := SaveLayoutCSV(fields, path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadLayoutCSV(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, fields) {
		t.Errorf("got %+v, want %+v", got, fields)
	}
}

func TestNormalizeType(t *testing.T) {
	tests := []struct {
		in     string
		ftype  string
		signed bool
	}{
		{"x", "X", false}, {" パック ", "P", false}, {"comp-3", "P", false},
		{"s9", "9", true}, {"SP", "P", true}, {"符号付き数値", "9", true},
	}
	for _, tt := range tests {
		ftype, signed, err := NormalizeType(tt.in)
		if err != nil || ftype != tt.ftype || signed != tt.signed {
			t.Errorf("%q: got %q %v %v", tt.in, ftype, signed, err)
		}
	}
	if _, _, err := NormalizeType("Q"); err == nil {
		t.Error("不明な形式がエラーになりません")
	}
}

func TestDetectAndGuessSample(t *testing.T) {
	path := filepath.Join("..", "sample.dat")
	rl, cands, err := DetectRecordLength(path, DefaultMaxRecordLength)
	if err != nil {
		t.Fatal(err)
	}
	if rl != 46 || len(cands) == 0 {
		t.Fatalf("rl = %d, cands = %v", rl, cands)
	}
	records, err := SampleRecords(path, rl, 1000)
	if err != nil {
		t.Fatal(err)
	}
	fields := GuessLayout(records)
	if len(fields) == 0 || RecordLengthOf(fields) != rl {
		t.Errorf("fields = %+v", fields)
	}
}

func TestConvertSample(t *testing.T) {
	nodes, err := ParseCopybook(filepath.Join("..", "sample.cpy"), "")
	if err != nil {
		t.Fatal(err)
	}
	fields, total := CopybookToFields(nodes)
	out := filepath.Join(t.TempDir(), "out.csv")
	res, err := Convert(ConvertOptions{
		Input: filepath.Join("..", "sample.dat"), Output: out, Fields: fields, RecordLength: total,
		CSV: true, Header: true, Limit: 2, Skip: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 2 || res.Errors.Count != 0 {
		t.Errorf("res = %+v", res)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), utf8BOM+"SHAIN-NO,NAME-KANA,") || strings.Count(string(data), "\r\n") != 3 {
		t.Errorf("out = %q", data)
	}
}

func TestConvertRefusesSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.dat")
	if err := os.WriteFile(path, []byte("12345"), 0o666); err != nil {
		t.Fatal(err)
	}
	_, err := Convert(ConvertOptions{Input: path, Output: path, RecordLength: 5,
		Fields: []Field{NewField("A", 0, 5, "9", 0, false)}})
	if err == nil {
		t.Fatal("同一ファイルへの出力がエラーになりません")
	}
	if data, _ := os.ReadFile(path); string(data) != "12345" {
		t.Errorf("入力が破壊されました: %q", data)
	}
}

func TestHexDump(t *testing.T) {
	got := HexDump([]byte{0x41, 0x00, 0x0A, 0xFF}, 2, 5)
	want := "===== レコード 1 =====\n" +
		"    1: 41 00                                           | A･\n\n" +
		"===== レコード 2 =====\n" +
		"    1: 0A FF                                           | ･\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestComma(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1000: "-1,000"} {
		if got := Comma(in); got != want {
			t.Errorf("%d: got %q, want %q", in, got, want)
		}
	}
}
