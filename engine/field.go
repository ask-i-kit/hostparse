// Package engine はホスト固定長ファイルのパース処理を提供する。
//
// SJIS・改行なしの固定長レコードファイルを、コピー句 / 手動レイアウト / 推測 に
// もとづいてパースし、テキストまたはCSVで出力する。パック項目(COMP-3)対応。
// CLI版・GUI版の共通エンジン。
package engine

import (
	"fmt"
	"math/big"
	"strings"
)

// Field はレコード内の1項目の定義。
type Field struct {
	Name   string
	Start  int    // 0始まりバイト位置
	Length int    // バイト数
	Type   string // X / K / N / 9 / P / B
	Scale  int    // 小数桁
	Signed bool
	Digits int // 数値の桁数 (表示幅計算用)
}

// NewField は Digits の既定値を補って Field を作る。
func NewField(name string, start, length int, ftype string, scale int, signed bool) Field {
	f := Field{Name: name, Start: start, Length: length, Type: ftype, Scale: scale, Signed: signed}
	f.fillDigits()
	return f
}

func (f *Field) fillDigits() {
	if f.Digits != 0 {
		return
	}
	switch f.Type {
	case "P":
		f.Digits = f.Length*2 - 1
	case "9", "B":
		f.Digits = f.Length
	}
}

// End はレコード先頭からの終端バイト位置 (0始まりの次位置) を返す。
func (f Field) End() int { return f.Start + f.Length }

// IsNumeric は数値項目 (右寄せ表示) かどうかを返す。
func (f Field) IsNumeric() bool {
	return f.Type == "9" || f.Type == "P" || f.Type == "B"
}

// TypeCode は GUI の形式コードを返す (符号付きゾーンのみ S9)。
func (f Field) TypeCode() string {
	if f.Type == "9" && f.Signed {
		return "S9"
	}
	return f.Type
}

var typeAliases = map[string]string{
	"X": "X", "半角": "X", "文字": "X", "CHAR": "X", "A": "X",
	"K": "K", "カナ": "K", "半角カナ": "K", "KANA": "K",
	"N": "N", "全角": "N", "漢字": "N", "ZENKAKU": "N",
	"9": "9", "数値": "9", "ゾーン": "9", "ZONE": "9", "ZD": "9",
	"S9": "S9", "符号付数値": "S9", "符号付き数値": "S9",
	"P": "P", "パック": "P", "PACK": "P", "COMP-3": "P", "PD": "P",
	"SP": "P", "S9P": "P",
	"B": "B", "バイナリ": "B", "BINARY": "B", "COMP": "B",
}

// NormalizeType は形式文字列を (ftype, signed) へ正規化する。
func NormalizeType(t string) (string, bool, error) {
	key := strip(t)
	if isASCII(key) {
		key = strings.ToUpper(key)
	}
	norm, ok := typeAliases[key]
	if !ok {
		return "", false, fmt.Errorf("不明な形式です: %q (使用可能: X,K,N,9,S9,P,B / 半角,カナ,全角,数値,パック,バイナリ)", t)
	}
	if norm == "S9" {
		return "9", true, nil
	}
	return norm, key == "SP" || key == "S9P", nil
}

// DisplayWidth はテキスト出力時のフィールド表示幅を返す。
func DisplayWidth(f Field) int {
	extra := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	switch f.Type {
	case "9":
		return f.Length + extra(f.Signed) + extra(f.Scale != 0)
	case "P":
		return f.Digits + 1 + extra(f.Scale != 0)
	case "B":
		n := new(big.Int).Lsh(big.NewInt(1), uint(8*f.Length))
		return len(n.String()) + 1
	}
	return f.Length
}

// RecordLengthOf はフィールド定義から導出したレコード長を返す。
func RecordLengthOf(fields []Field) int {
	max := 0
	for _, f := range fields {
		if f.End() > max {
			max = f.End()
		}
	}
	return max
}

// FieldsOverRecord はレコード長を超えるフィールドを返す。
func FieldsOverRecord(fields []Field, recordLength int) []Field {
	var over []Field
	for _, f := range fields {
		if f.End() > recordLength {
			over = append(over, f)
		}
	}
	return over
}

// DescribeOver は超過フィールドを先頭5件まで「名前(終端N)」形式で列挙する。
func DescribeOver(over []Field) string {
	if len(over) > 5 {
		over = over[:5]
	}
	parts := make([]string, len(over))
	for i, f := range over {
		parts[i] = fmt.Sprintf("%s(終端%d)", f.Name, f.End())
	}
	return strings.Join(parts, ", ")
}
