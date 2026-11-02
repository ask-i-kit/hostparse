// make-sample — テスト用サンプルデータ生成 (SJIS・改行なし・パック項目あり)
//
// 使い方: make-sample [件数] [出力ファイル]
package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"hostparse/engine"
)

const copybook = `       01  SHAIN-REC.
           05  SHAIN-NO      PIC 9(5).
           05  NAME-KANA     PIC X(10).
           05  NAME-KANJI    PIC N(5).
           05  KYUYO         PIC S9(7)V99 COMP-3.
           05  BUSHO-CD      PIC S9(4)    COMP.
           05  NYUSHA-YMD    PIC 9(8).
           05  BIKO          PIC X(6).
`

// レコード長 = 5 + 10 + 10 + 5 + 2 + 8 + 6 = 46 バイト
const recordLength = 46

var (
	kana  = []string{"ﾀﾅｶ ﾀﾛｳ", "ｽｽﾞｷ ﾊﾅｺ", "ｻﾄｳ ｼﾞﾛｳ", "ﾔﾏﾀﾞ ｹﾝ", "ｲﾄｳ ﾕｷ"}
	kanji = []string{"田中太郎", "鈴木花子", "佐藤次郎", "山田健", "伊藤幸"}
)

// pack は符号付きパック10進 (COMP-3) にエンコードする。
func pack(value, digits int) []byte {
	sign := "c"
	if value < 0 {
		sign, value = "d", -value
	}
	s := fmt.Sprintf("%0*d", digits, value)
	if len(s)%2 == 0 { // ニブル数 = 桁数+1(符号) が偶数になるよう調整
		s = "0" + s
	}
	b, err := hex.DecodeString(s + sign)
	if err != nil {
		panic(err)
	}
	return b
}

// fit は s を CP932 にして空白埋めで n バイトちょうどにする。
func fit(s string, n int) []byte {
	b := engine.AppendCP932(nil, s)
	if len(b) < n {
		b = append(b, bytes.Repeat([]byte{' '}, n-len(b))...)
	}
	return b[:n]
}

func makeRecord(rng *rand.Rand, i int) []byte {
	k := rng.Intn(len(kana))
	var rec []byte
	rec = append(rec, fmt.Sprintf("%05d", i%100000)...) // SHAIN-NO 9(5)
	rec = append(rec, fit(kana[k], 10)...)              // NAME-KANA X(10)
	name := kanji[k] + strings.Repeat("　", 5-utf8.RuneCountInString(kanji[k]))
	rec = append(rec, fit(name, 10)...)                     // NAME-KANJI N(5)
	rec = append(rec, pack(rng.Intn(10099999)-99999, 9)...) // KYUYO S9(7)V99 COMP-3 (9桁 -> 5バイト)
	busho := rng.Intn(9999) + 1
	rec = append(rec, byte(busho>>8), byte(busho)) // BUSHO-CD COMP 2バイト
	rec = append(rec, fmt.Sprintf("%d%02d%02d", 1990+rng.Intn(37), 1+rng.Intn(12), 1+rng.Intn(28))...)
	rec = append(rec, fit("ﾋﾞｺｳ", 6)...) // BIKO X(6)
	if len(rec) != recordLength {
		panic(fmt.Sprintf("レコード長が不正です: %d", len(rec)))
	}
	return rec
}

func main() {
	n, out := 100, "sample.dat"
	if len(os.Args) > 1 {
		v, err := strconv.Atoi(os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "件数が不正です:", os.Args[1])
			os.Exit(2)
		}
		n = v
	}
	if len(os.Args) > 2 {
		out = os.Args[2]
	}

	rng := rand.New(rand.NewSource(42))
	data := make([]byte, 0, n*recordLength)
	for i := 1; i <= n; i++ {
		data = append(data, makeRecord(rng, i)...)
	}
	if err := os.WriteFile(out, data, 0o666); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	crlf := strings.ReplaceAll(copybook, "\n", "\r\n")
	if err := os.WriteFile("sample.cpy", []byte(crlf), 0o666); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s (%d レコード, %d バイト/レコード) と sample.cpy を生成しました\n", out, n, recordLength)
}
