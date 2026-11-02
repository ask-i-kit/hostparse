// hostparse — ホスト固定長ファイル パースツール (CLI版)
//
// SJIS・改行なしの固定長レコードファイルを、コピー句 / 手動レイアウト / 推測 に
// もとづいてパースし、テキストまたはCSVで出力する。パック項目(COMP-3)対応。
package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"hostparse/engine"
)

const usage = `使い方: hostparse [オプション] 入力ファイル

ホスト固定長ファイル(SJIS・改行なし)をパースしてテキスト/CSVに変換する。

レイアウト指定 (いずれか1つ):
  -c, --copybook FILE       コピー句ファイル (COBOL COPY句)
  -l, --layout FILE         レイアウトCSVファイル (名前,開始,バイト数,形式,小数桁)
  -f, --field SPEC          フィールド直接指定 "名前,開始,バイト数,形式[,小数桁]" (複数指定可)
  -g, --guess               レイアウトを推測する (レイアウト案をCSV保存)

出力:
  -o, --output FILE         出力ファイル (省略時: 標準出力)
      --format {text,csv}   出力形式 (省略時: 出力ファイルの拡張子から判断, 既定 text)
      --encoding {utf-8,ansi}
                            出力文字コード (ansi=CP932/Shift_JIS, 既定 utf-8)
      --delimiter STR       text形式の項目区切り (既定: 半角空白)
      --header              text形式でも見出し行を出力する
      --no-header           csv形式で見出し行を出力しない
      --keep-zeros          数値の先頭ゼロを残す (既定: 除去して 00123 -> 123)

その他:
  -r, --record-length N     レコード長(バイト)。コピー句指定時は省略可
      --limit N             先頭 N レコードのみ処理
      --skip N              先頭 N レコードを読み飛ばす
      --show-layout         レイアウトを表示して終了 (変換しない)
      --save-layout FILE    解決したレイアウトをCSVで保存する
      --copybook-encoding ENC
                            コピー句ファイルの文字コード (既定: 自動判定)
  -h, --help                このヘルプを表示する

使用例:
  # コピー句からパースして CSV (UTF-8) 出力
  hostparse data.dat -c copybook.cpy -o out.csv

  # 手動レイアウト(CSV)でテキスト(ANSI=CP932)出力
  hostparse data.dat -l layout.csv --format text --encoding ansi -o out.txt

  # コマンドラインでフィールドを直接指定 (名前,開始位置(1始まり),バイト数,形式[,小数桁])
  hostparse data.dat --field "社員番号,1,5,9" --field "金額,16,5,P,2" -o out.csv

  # レイアウトを推測 (レコード長も自動検出) してレイアウト案を保存
  hostparse data.dat --guess

形式コード:
  X  … 半角文字 (英数記号)        K  … 半角カナ
  N  … 全角文字                   9  … ゾーン数値 (S9 で符号付き)
  P  … パック10進 (COMP-3)        B  … バイナリ (COMP)
`

type options struct {
	input            string
	copybook         string
	layout           string
	fields           []string
	guess            bool
	output           string
	format           string
	encoding         string
	delimiter        string
	header           bool
	noHeader         bool
	keepZeros        bool
	recordLength     int
	hasRecordLength  bool
	limit            int64
	skip             int64
	showLayout       bool
	saveLayout       string
	copybookEncoding string
}

// usageError は使い方の誤り (終了コード 2) を表す。
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usageErrorf(format string, a ...any) error {
	return usageError{fmt.Sprintf(format, a...)}
}

var errHelp = errors.New("help")

// flagSpec はオプション1つの定義。value が nil なら引数なしのフラグ。
type flagSpec struct {
	long  string
	short string
	flag  *bool
	value func(string) error
}

func intValue[T int | int64](name string, dst *T, seen *bool) func(string) error {
	return func(s string) error {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return usageErrorf("%s の値が整数ではありません: %s", name, s)
		}
		*dst = T(v)
		if seen != nil {
			*seen = true
		}
		return nil
	}
}

func stringValue(dst *string) func(string) error {
	return func(s string) error {
		*dst = s
		return nil
	}
}

func choiceValue(name string, dst *string, choices ...string) func(string) error {
	return func(s string) error {
		for _, c := range choices {
			if s == c {
				*dst = s
				return nil
			}
		}
		return usageErrorf("%s の値が不正です: %s (選択肢: %s)", name, s, strings.Join(choices, ", "))
	}
}

var negativeNumberRe = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// looksLikeOption は引数がオプションの形をしているかどうかを判定する。
func looksLikeOption(a string) bool {
	return len(a) > 1 && a[0] == '-' && !negativeNumberRe.MatchString(a) && !strings.Contains(a, " ")
}

func parseArgs(argv []string) (*options, error) {
	o := &options{encoding: "utf-8", delimiter: " "}
	var help bool
	specs := []flagSpec{
		{long: "copybook", short: "c", value: stringValue(&o.copybook)},
		{long: "layout", short: "l", value: stringValue(&o.layout)},
		{long: "field", short: "f", value: func(s string) error {
			o.fields = append(o.fields, s)
			return nil
		}},
		{long: "guess", short: "g", flag: &o.guess},
		{long: "output", short: "o", value: stringValue(&o.output)},
		{long: "format", value: choiceValue("--format", &o.format, "text", "csv")},
		{long: "encoding", value: choiceValue("--encoding", &o.encoding, "utf-8", "ansi")},
		{long: "delimiter", value: stringValue(&o.delimiter)},
		{long: "header", flag: &o.header},
		{long: "no-header", flag: &o.noHeader},
		{long: "keep-zeros", flag: &o.keepZeros},
		{long: "record-length", short: "r", value: intValue("--record-length", &o.recordLength, &o.hasRecordLength)},
		{long: "limit", value: intValue("--limit", &o.limit, nil)},
		{long: "skip", value: intValue("--skip", &o.skip, nil)},
		{long: "show-layout", flag: &o.showLayout},
		{long: "save-layout", value: stringValue(&o.saveLayout)},
		{long: "copybook-encoding", value: stringValue(&o.copybookEncoding)},
		{long: "help", short: "h", flag: &help},
	}

	// 長いオプションは一意に定まる範囲で省略できる
	findLong := func(name string) (*flagSpec, error) {
		var found []*flagSpec
		for i := range specs {
			if specs[i].long == name {
				return &specs[i], nil
			}
			if strings.HasPrefix(specs[i].long, name) {
				found = append(found, &specs[i])
			}
		}
		switch len(found) {
		case 0:
			return nil, usageErrorf("不明なオプションです: --%s", name)
		case 1:
			return found[0], nil
		}
		return nil, usageErrorf("オプションがあいまいです: --%s", name)
	}
	findShort := func(c string) (*flagSpec, error) {
		for i := range specs {
			if specs[i].short == c {
				return &specs[i], nil
			}
		}
		return nil, usageErrorf("不明なオプションです: -%s", c)
	}

	var positional []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		// 次の引数をオプションの値として取り出す
		next := func(name string) (string, error) {
			if i+1 >= len(argv) || argv[i+1] == "--" || looksLikeOption(argv[i+1]) {
				return "", usageErrorf("%s には値が必要です", name)
			}
			i++
			return argv[i], nil
		}
		switch {
		case a == "--":
			positional = append(positional, argv[i+1:]...)
			i = len(argv)
		case strings.HasPrefix(a, "--") && !strings.Contains(a, " "):
			name, val, hasVal := strings.Cut(a[2:], "=")
			spec, err := findLong(name)
			if err != nil {
				return nil, err
			}
			if spec.flag != nil {
				if hasVal {
					return nil, usageErrorf("--%s は値を取りません", spec.long)
				}
				*spec.flag = true
				break
			}
			if !hasVal {
				if val, err = next("--" + spec.long); err != nil {
					return nil, err
				}
			}
			if err := spec.value(val); err != nil {
				return nil, err
			}
		case looksLikeOption(a):
			// 短いオプション: -g / -o FILE / -oFILE / -gr46 のような連結にも対応
			for rest := a[1:]; rest != ""; {
				spec, err := findShort(rest[:1])
				if err != nil {
					return nil, err
				}
				rest = rest[1:]
				if spec.flag != nil {
					*spec.flag = true
					continue
				}
				val := strings.TrimPrefix(rest, "=")
				if rest == "" {
					if val, err = next("-" + spec.short); err != nil {
						return nil, err
					}
				}
				if err := spec.value(val); err != nil {
					return nil, err
				}
				break
			}
		default:
			positional = append(positional, a)
		}
		if help {
			return nil, errHelp
		}
	}

	switch {
	case len(positional) == 0:
		return nil, usageErrorf("入力ファイルを指定してください")
	case len(positional) > 1:
		return nil, usageErrorf("余分な引数です: %s", strings.Join(positional[1:], " "))
	}
	o.input = positional[0]
	return o, nil
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

func run(argv []string) error {
	o, err := parseArgs(argv)
	if err != nil {
		return err
	}
	if st, err := os.Stat(o.input); err != nil || st.IsDir() {
		return usageErrorf("入力ファイルが見つかりません: %s", o.input)
	}

	nSrc := 0
	for _, used := range []bool{o.copybook != "", o.layout != "", len(o.fields) > 0, o.guess} {
		if used {
			nSrc++
		}
	}
	if nSrc == 0 {
		return usageErrorf("レイアウトを指定してください: -c コピー句 / -l レイアウトCSV / --field / --guess")
	}
	if nSrc > 1 {
		return usageErrorf("レイアウト指定 (-c / -l / --field / --guess) は1つだけにしてください")
	}
	if o.hasRecordLength && o.recordLength <= 0 {
		return usageErrorf("--record-length は1以上で指定してください")
	}
	if o.skip < 0 {
		return usageErrorf("--skip は0以上で指定してください")
	}

	// --- レイアウト解決 ---
	var fields []engine.Field
	recordLength := o.recordLength
	switch {
	case o.copybook != "":
		nodes, err := engine.ParseCopybook(o.copybook, o.copybookEncoding)
		if err != nil {
			return err
		}
		var total int
		fields, total = engine.CopybookToFields(nodes)
		if len(fields) == 0 {
			return usageErrorf("コピー句からフィールドを取得できませんでした")
		}
		if !o.hasRecordLength {
			recordLength = total
		} else if recordLength < total {
			return usageErrorf("--record-length (%d) がコピー句の合計 (%d) より小さいです", recordLength, total)
		}
	case o.layout != "" || len(o.fields) > 0:
		if o.layout != "" {
			fields, err = engine.LoadLayoutCSV(o.layout)
		} else {
			fields, err = engine.ParseFieldArgs(o.fields)
		}
		if err != nil {
			return err
		}
		if len(fields) == 0 {
			return errors.New("レイアウトに項目がありません")
		}
		if !o.hasRecordLength {
			recordLength = engine.RecordLengthOf(fields)
		}
	default: // guess
		if !o.hasRecordLength {
			warnf("レコード長を自動検出しています...")
			rl, cands, err := engine.DetectRecordLength(o.input, engine.DefaultMaxRecordLength)
			if err != nil {
				return err
			}
			if rl == 0 {
				return usageErrorf("レコード長を検出できませんでした。--record-length で指定してください")
			}
			recordLength = rl
			warnf("レコード長: %d バイトと推定 (候補: %s)", rl, engine.FormatCandidates(cands))
		}
		records, err := engine.SampleRecords(o.input, recordLength, 1000)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return usageErrorf("ファイルが1レコード (%d バイト) 未満です", recordLength)
		}
		fields = engine.GuessLayout(records)
		layoutPath := o.saveLayout
		if layoutPath == "" {
			layoutPath = o.input + ".layout.csv"
		}
		if err := engine.SaveLayoutCSV(fields, layoutPath); err != nil {
			return err
		}
		warnf("推測レイアウトを保存しました: %s", layoutPath)
		warnf("※ 推測結果です。保存されたCSVを確認・修正のうえ -l で再実行することを推奨します。")
	}

	// 出力形式の決定
	if o.format == "" {
		o.format = "text"
		if strings.HasSuffix(strings.ToLower(o.output), ".csv") {
			o.format = "csv"
		}
	}

	warnf("%s", engine.FormatLayout(fields, recordLength))
	if o.saveLayout != "" && !o.guess {
		if err := engine.SaveLayoutCSV(fields, o.saveLayout); err != nil {
			return err
		}
		warnf("レイアウトを保存しました: %s", o.saveLayout)
	}
	if o.showLayout {
		return nil
	}

	// レコード長の妥当性チェック (レイアウトから導出した場合はズレを許さない)
	st, err := os.Stat(o.input)
	if err != nil {
		return err
	}
	size, rl := st.Size(), int64(recordLength)
	if size%rl != 0 && !o.hasRecordLength && (o.layout != "" || len(o.fields) > 0) {
		return usageErrorf("ファイルサイズ %s がレイアウトから導出したレコード長 %d の倍数ではありません。"+
			"フィールドがレコードの一部しかカバーしていない可能性があります。"+
			"-r/--record-length で実際のレコード長を指定してください", engine.Comma(size), recordLength)
	}

	// 範囲チェック
	if over := engine.FieldsOverRecord(fields, recordLength); len(over) > 0 {
		return usageErrorf("レコード長 %d を超えるフィールドがあります: %s", recordLength, engine.DescribeOver(over))
	}

	if size%rl != 0 {
		warnf("警告: ファイルサイズ %d がレコード長 %d の倍数ではありません (末尾 %d バイトは無視されます)",
			size, recordLength, size%rl)
	}

	isCSV := o.format == "csv"
	res, err := engine.Convert(engine.ConvertOptions{
		Input:        o.input,
		Output:       o.output,
		Stdout:       os.Stdout,
		Fields:       fields,
		RecordLength: recordLength,
		CSV:          isCSV,
		ANSI:         o.encoding == "ansi",
		Delimiter:    o.delimiter,
		Header:       isCSV && !o.noHeader || !isCSV && o.header,
		KeepZeros:    o.keepZeros,
		Limit:        o.limit,
		Skip:         o.skip,
	})
	if err != nil {
		return err
	}

	warnf("完了: %s レコードを処理しました。", engine.Comma(res.Records))
	if res.Errors.Count != 0 {
		warnf("警告: 変換エラー %s 件 (最初: 項目=%s 値=0x%s)。 出力中は #ERR:<hex> と表示されています。レイアウトを確認してください。",
			engine.Comma(res.Errors.Count), res.Errors.FirstName, res.Errors.FirstValue)
	}
	return nil
}

func main() {
	err := run(os.Args[1:])
	var ue usageError
	switch {
	case err == nil:
	case errors.Is(err, errHelp):
		fmt.Print(usage)
	case errors.As(err, &ue):
		warnf("hostparse: エラー: %s", ue.msg)
		warnf("詳しくは hostparse --help を参照してください")
		os.Exit(2)
	default:
		warnf("hostparse: エラー: %v", err)
		os.Exit(1)
	}
}
