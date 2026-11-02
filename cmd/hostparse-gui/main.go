// hostparse-gui — ホスト固定長ファイル パースツール (GUI版)
//
// SJIS・改行なしの固定長レコードファイルを、コピー句 / 手動レイアウト / 推測 に
// もとづいてパースし、テキストまたはCSVで出力する。エンジンは engine パッケージを利用。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"hostparse/engine"
)

// 形式コード <-> 表示ラベル
var typeLabels = []struct{ code, label string }{
	{"X", "X: 半角文字"},
	{"K", "K: 半角カナ"},
	{"N", "N: 全角文字"},
	{"9", "9: ゾーン数値"},
	{"S9", "S9: 符号付数値"},
	{"P", "P: パック(COMP-3)"},
	{"B", "B: バイナリ(COMP)"},
}

const (
	previewRecords = 200
	dumpRecords    = 5   // 16進ダンプに表示するレコード数
	dumpBytes      = 512 // レコード長が未確定のときにダンプするバイト数
	progressMax    = 1000
)

func typeIndex(code string) int {
	for i, t := range typeLabels {
		if t.code == code {
			return i
		}
	}
	return 0
}

func typeLabelOf(f engine.Field) string {
	if i := typeIndex(f.TypeCode()); typeLabels[i].code == f.TypeCode() {
		return typeLabels[i].label
	}
	return f.Type
}

// rowModel は文字列の行データを保持する TableView 用モデル。
type rowModel struct {
	walk.TableModelBase
	rows [][]string
}

func (m *rowModel) RowCount() int { return len(m.rows) }

func (m *rowModel) Value(row, col int) interface{} {
	if row >= len(m.rows) || col >= len(m.rows[row]) {
		return ""
	}
	return engine.Printable(m.rows[row][col])
}

func (m *rowModel) setRows(rows [][]string) {
	m.rows = rows
	m.PublishRowsReset()
}

type app struct {
	mw *walk.MainWindow

	edInput, edRecLen, edOutput         *walk.LineEdit
	edName, edStart, edLen, edScale     *walk.LineEdit
	cbType                              *walk.ComboBox
	lblFileInfo, lblPrevInfo, lblStatus *walk.Label
	tvLayout, tvPreview                 *walk.TableView
	teDump                              *walk.TextEdit
	tabs                                *walk.TabWidget
	rbCSV, rbText, rbUTF8, rbANSI       *walk.RadioButton
	chkKeepZeros                        *walk.CheckBox
	progress                            *walk.ProgressBar
	btnConvert                          *walk.PushButton
	busyButtons                         [6]*walk.PushButton // 処理中に無効化するボタン

	layoutModel  *rowModel
	previewModel *rowModel

	fields         []engine.Field
	converting     bool
	inputSize      int64           // 入力ファイルサイズ (パス変更時のみ取得, 未取得は -1)
	bgRunning      map[string]bool // 実行中のバックグラウンド処理キー
	prevColsSig    string          // プレビューの列構成 (変化時のみ組み直す)
	previewStatus  string          // プレビュー完了時に出すステータス (上書き用)
	guessAfterDet  bool            // 自動検出の完了後に推測へ続けるか
	inputReloadGen int             // 入力パス変更のデバウンス用
}

// --------------------------------------------------- バックグラウンド実行

// runBG は fn をワーカーで実行する。
//
// fn はファイルI/Oと純粋計算のみを行い、ウィジェットには一切触れない。
// 結果は UIスレッド上の done が受け取る。
func (a *app) runBG(key string, fn func() (any, error), busyMsg string,
	done func(result any, err error, elapsed float64)) bool {
	if a.bgRunning[key] {
		return false // 同じ処理の多重実行は無視する
	}
	a.bgRunning[key] = true
	a.updateBusy()
	a.setStatus(busyMsg)

	go func() {
		t0 := time.Now()
		var result any
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("内部エラー: %v", r)
				}
			}()
			result, err = fn()
		}()
		elapsed := time.Since(t0).Seconds()
		a.mw.Synchronize(func() {
			delete(a.bgRunning, key)
			a.updateBusy()
			done(result, err, elapsed)
		})
	}()
	return true
}

// updateBusy は処理中にカーソルを待機表示にし、ファイルを触るボタンを止める。
func (a *app) updateBusy() {
	busy := len(a.bgRunning) > 0
	var cursor walk.Cursor
	if busy {
		cursor = walk.CursorWait()
	}
	setCursorAll(a.mw, cursor)
	for _, btn := range a.busyButtons {
		btn.SetEnabled(!busy)
	}
}

func setCursorAll(c walk.Container, cursor walk.Cursor) {
	c.AsContainerBase().SetCursor(cursor)
	children := c.Children()
	for i := 0; i < children.Len(); i++ {
		w := children.At(i)
		w.SetCursor(cursor)
		if sub, ok := w.(walk.Container); ok {
			setCursorAll(sub, cursor)
		}
	}
}

func (a *app) setStatus(s string) {
	// Label は & をニーモニックとして解釈する
	a.lblStatus.SetText(strings.ReplaceAll(s, "&", "&&"))
}

func (a *app) warn(title, msg string) {
	walk.MsgBox(a.mw, title, msg, walk.MsgBoxOK|walk.MsgBoxIconWarning)
}

func (a *app) info(title, msg string) {
	walk.MsgBox(a.mw, title, msg, walk.MsgBoxOK|walk.MsgBoxIconInformation)
}

func (a *app) fail(title, msg string) {
	walk.MsgBox(a.mw, title, msg, walk.MsgBoxOK|walk.MsgBoxIconError)
}

func (a *app) confirm(title, msg string) bool {
	return walk.MsgBox(a.mw, title, msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) == win.IDYES
}

// ------------------------------------------------------------------ UI

func labeled(text string) Widget { return Label{Text: text} }

func (a *app) build() error {
	busyButton := func(i int, text string, onClicked walk.EventHandler) PushButton {
		return PushButton{AssignTo: &a.busyButtons[i], Text: text, OnClicked: onClicked}
	}
	typeNames := make([]string, len(typeLabels))
	for i, t := range typeLabels {
		typeNames[i] = t.label
	}

	return MainWindow{
		AssignTo: &a.mw,
		Title:    "hostparse — ホスト固定長ファイル パースツール",
		Size:     Size{Width: 1180, Height: 800},
		MinSize:  Size{Width: 900, Height: 620},
		Font:     Font{Family: "Meiryo UI", PointSize: 9},
		Layout:   VBox{Margins: Margins{Left: 8, Top: 8, Right: 8, Bottom: 4}, Spacing: 4},
		Children: []Widget{
			// ===== 入力ファイル =====
			GroupBox{
				Title:  "入力ファイル (SJIS・改行なし固定長)",
				Layout: VBox{},
				Children: []Widget{
					// GroupBox 直下の LineEdit には Walk の不具合で変更通知が届かない。
					// Edit は生成時の親へ通知し続けるが、GroupBox が転送先を探せないため、
					// Composite を挟んで最初から正しい親の下に作る。
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 4},
						Children: []Widget{
							LineEdit{AssignTo: &a.edInput, OnTextChanged: func() { a.scheduleInputReload(300 * time.Millisecond) }},
							busyButton(0, "参照...", a.pickInput),
							labeled("レコード長:"),
							LineEdit{AssignTo: &a.edRecLen, MinSize: Size{Width: 70}, MaxSize: Size{Width: 70}, OnTextChanged: a.updateFileInfo},
							busyButton(1, "自動検出", a.detectRecLen),
						},
					},
					Label{AssignTo: &a.lblFileInfo, Text: "ファイル未選択", EllipsisMode: EllipsisEnd},
				},
			},

			// ===== 中央: レイアウト(左) + プレビュー(右) =====
			HSplitter{
				StretchFactor: 1,
				Children: []Widget{
					GroupBox{
						Title:         "レイアウト",
						StretchFactor: 2,
						Layout:        VBox{Spacing: 4},
						Children: []Widget{
							Composite{
								Layout: HBox{MarginsZero: true, Spacing: 4},
								Children: []Widget{
									busyButton(2, "コピー句 読込", a.loadCopybook),
									busyButton(3, "レイアウトCSV 読込", a.loadLayout),
									busyButton(4, "推測", a.guess),
									PushButton{Text: "CSV保存", OnClicked: a.saveLayout},
									HSpacer{},
									PushButton{Text: "全クリア", OnClicked: a.clearLayout},
								},
							},
							TableView{
								AssignTo:         &a.tvLayout,
								Model:            a.layoutModel,
								AlternatingRowBG: true,
								Columns: []TableViewColumn{
									{Title: "名前", Width: 140},
									{Title: "開始", Width: 45, Alignment: AlignFar},
									{Title: "バイト", Width: 50, Alignment: AlignFar},
									{Title: "形式", Width: 120},
									{Title: "小数", Width: 40, Alignment: AlignFar},
									{Title: "終端", Width: 45, Alignment: AlignFar},
								},
								OnCurrentIndexChanged: a.onLayoutSelect,
							},
						},
					},
					TabWidget{
						AssignTo:      &a.tabs,
						StretchFactor: 3,
						Pages: []TabPage{
							{
								Title:  fmt.Sprintf("プレビュー (先頭%d件)", previewRecords),
								Layout: VBox{Margins: Margins{Left: 2, Top: 2, Right: 2, Bottom: 2}, Spacing: 2},
								Children: []Widget{
									Composite{
										Layout: HBox{MarginsZero: true, Spacing: 8},
										Children: []Widget{
											busyButton(5, "プレビュー更新", func() { a.refreshPreview("") }),
											Label{AssignTo: &a.lblPrevInfo},
											HSpacer{},
										},
									},
									TableView{
										AssignTo:         &a.tvPreview,
										Model:            a.previewModel,
										AlternatingRowBG: true,
									},
								},
							},
							{
								Title:  "16進ダンプ",
								Layout: VBox{Margins: Margins{Left: 2, Top: 2, Right: 2, Bottom: 2}},
								Children: []Widget{
									TextEdit{
										AssignTo: &a.teDump,
										ReadOnly: true,
										HScroll:  true,
										VScroll:  true,
										Font:     Font{Family: "MS Gothic", PointSize: 10},
									},
								},
							},
						},
					},
				},
			},

			// ===== フィールド編集 =====
			GroupBox{
				Title:  "フィールド編集 (行を選択→修正→更新 / 新規は追加)",
				Layout: HBox{Spacing: 4},
				Children: []Widget{
					labeled("名前"),
					LineEdit{AssignTo: &a.edName, MinSize: Size{Width: 130}, MaxSize: Size{Width: 130}},
					labeled("開始(1始まり)"),
					LineEdit{AssignTo: &a.edStart, MinSize: Size{Width: 50}, MaxSize: Size{Width: 50}},
					labeled("バイト数"),
					LineEdit{AssignTo: &a.edLen, MinSize: Size{Width: 50}, MaxSize: Size{Width: 50}},
					labeled("形式"),
					ComboBox{AssignTo: &a.cbType, Model: typeNames, CurrentIndex: 0, MinSize: Size{Width: 130}},
					labeled("小数桁"),
					LineEdit{AssignTo: &a.edScale, MinSize: Size{Width: 40}, MaxSize: Size{Width: 40}},
					PushButton{Text: "追加", OnClicked: a.addField},
					PushButton{Text: "更新", OnClicked: a.updateField},
					PushButton{Text: "削除", OnClicked: a.deleteField},
					HSpacer{},
				},
			},

			// ===== 出力 =====
			GroupBox{
				Title:  "出力",
				Layout: VBox{Spacing: 4},
				Children: []Widget{
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 8},
						Children: []Widget{
							RadioButton{AssignTo: &a.rbCSV, Text: "CSV"},
							RadioButton{AssignTo: &a.rbText, Text: "テキスト(桁揃え)"},
							HSpacer{Size: 24}, // ラジオボタンのグループはここで分かれる
							RadioButton{AssignTo: &a.rbUTF8, Text: "UTF-8"},
							RadioButton{AssignTo: &a.rbANSI, Text: "ANSI (SJIS)"},
							CheckBox{AssignTo: &a.chkKeepZeros, Text: "先頭ゼロを残す"},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 4},
						Children: []Widget{
							LineEdit{AssignTo: &a.edOutput},
							PushButton{Text: "参照...", OnClicked: a.pickOutput},
							PushButton{AssignTo: &a.btnConvert, Text: "変換実行", OnClicked: a.convert},
						},
					},
					ProgressBar{AssignTo: &a.progress, MinValue: 0, MaxValue: progressMax},
				},
			},

			// ===== ステータスバー =====
			Label{AssignTo: &a.lblStatus, Text: "入力ファイルを選択してください", EllipsisMode: EllipsisEnd},
		},
	}.Create()
}

// ------------------------------------------------------------ 入力ファイル

func (a *app) pickInput() {
	dlg := walk.FileDialog{
		Title:  "入力ファイルを選択",
		Filter: "すべてのファイル (*.*)|*.*|データ (*.dat;*.txt;*.bin)|*.dat;*.txt;*.bin",
	}
	if ok, err := dlg.ShowOpen(a.mw); err != nil || !ok {
		return
	}
	a.edInput.SetText(dlg.FilePath)
	base := strings.TrimSuffix(dlg.FilePath, filepath.Ext(dlg.FilePath))
	a.edOutput.SetText(base + "_変換.csv")
	a.scheduleInputReload(0) // 明示的な選択なので待たない
}

// scheduleInputReload は入力パスの変更をまとめて処理する (直接入力中にファイルを叩かない)。
func (a *app) scheduleInputReload(delay time.Duration) {
	a.inputReloadGen++
	a.inputSize = -1
	a.reloadAfter(delay)
}

func (a *app) reloadAfter(delay time.Duration) {
	gen := a.inputReloadGen
	time.AfterFunc(delay, func() {
		a.mw.Synchronize(func() {
			if gen == a.inputReloadGen {
				a.reloadInput()
			}
		})
	})
}

func (a *app) reloadInput() {
	path := strings.TrimSpace(a.edInput.Text())
	if path == "" {
		a.lblFileInfo.SetText("ファイル未選択")
		return
	}
	a.lblFileInfo.SetText("ファイル情報を読込んでいます...")
	// ネットワークドライブやクラウド同期フォルダでは stat/read 自体が
	// ブロックしうるため、サイズ取得もダンプもワーカーで行う。
	ok := a.runBG("fileinfo", func() (any, error) {
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			return nil, fmt.Errorf("フォルダです: %s", path)
		}
		return st.Size(), nil
	}, "ファイル情報を読込んでいます...", a.onFileInfo)
	if !ok {
		a.reloadAfter(200 * time.Millisecond)
	}
}

func (a *app) onFileInfo(size any, err error, _ float64) {
	if err != nil {
		a.inputSize = -1
		a.lblFileInfo.SetText("ファイルを読めません")
		a.setStatus(fmt.Sprintf("入力ファイルを読めません: %v", err))
		return
	}
	a.inputSize = size.(int64)
	a.updateFileInfo()
	a.refreshDump()
	a.setStatus("レイアウトを指定してください (コピー句読込 / 推測 / 手動追加)")
}

// updateFileInfo はキャッシュ済みサイズから表示を組み立てる (ファイルには触らない)。
func (a *app) updateFileInfo() {
	if a.inputSize < 0 {
		if strings.TrimSpace(a.edInput.Text()) != "" {
			a.lblFileInfo.SetText("ファイル情報を読込んでいます...")
		} else {
			a.lblFileInfo.SetText("ファイル未選択")
		}
		return
	}
	size := a.inputSize
	if rl, ok := a.getRecLen(true); ok {
		warn := ""
		if rem := size % int64(rl); rem != 0 {
			warn = fmt.Sprintf(" ※末尾%dバイト余り", rem)
		}
		a.lblFileInfo.SetText(fmt.Sprintf("サイズ: %s バイト / %s レコード%s",
			engine.Comma(size), engine.Comma(size/int64(rl)), warn))
	} else {
		a.lblFileInfo.SetText(fmt.Sprintf("サイズ: %s バイト", engine.Comma(size)))
	}
}

func (a *app) getRecLen(silent bool) (int, bool) {
	s := strings.TrimSpace(a.edRecLen.Text())
	if s == "" {
		if !silent {
			a.warn("レコード長", "レコード長を入力するか自動検出してください")
		}
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		if !silent {
			a.fail("レコード長", "レコード長が不正です: "+s)
		}
		return 0, false
	}
	return v, true
}

func (a *app) needInput() (string, bool) {
	path := a.edInput.Text()
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		a.warn("入力ファイル", "入力ファイルを選択してください")
		return "", false
	}
	return path, true
}

type detectResult struct {
	recordLength int
	cands        []engine.Candidate
}

func detectWorker(path string) func() (any, error) {
	return func() (any, error) {
		rl, cands, err := engine.DetectRecordLength(path, engine.DefaultMaxRecordLength)
		return detectResult{rl, cands}, err
	}
}

func (a *app) detectRecLen() {
	if path, ok := a.needInput(); ok {
		a.runBG("detect", detectWorker(path), "レコード長を検出しています...", a.onDetect)
	}
}

func (a *app) onDetect(result any, err error, elapsed float64) {
	chainGuess := a.guessAfterDet
	a.guessAfterDet = false
	if err != nil {
		a.setStatus(fmt.Sprintf("レコード長の検出に失敗しました: %v", err))
		a.fail("自動検出", fmt.Sprintf("レコード長を検出できませんでした:\n%v", err))
		return
	}
	r := result.(detectResult)
	if r.recordLength == 0 {
		a.setStatus("レコード長の検出に失敗しました")
		a.warn("自動検出", "レコード長を検出できませんでした。\n"+
			"ファイルサイズがレコード長の倍数でない可能性があります。手入力してください。")
		return
	}
	a.edRecLen.SetText(strconv.Itoa(r.recordLength))
	a.updateFileInfo()
	a.refreshDump()
	if chainGuess {
		a.guess()
		return
	}
	// 後続処理のメッセージを消さないよう最後に表示
	a.setStatus(fmt.Sprintf("レコード長 %d バイトと推定 (候補: %s) / %.2f秒",
		r.recordLength, engine.FormatCandidates(r.cands), elapsed))
}

// ------------------------------------------------------------ レイアウト

func (a *app) refreshLayoutTable() {
	rows := make([][]string, len(a.fields))
	for i, f := range a.fields {
		scale := ""
		if f.Scale != 0 {
			scale = strconv.Itoa(f.Scale)
		}
		rows[i] = []string{f.Name, strconv.Itoa(f.Start + 1), strconv.Itoa(f.Length),
			typeLabelOf(f), scale, strconv.Itoa(f.End())}
	}
	a.layoutModel.setRows(rows)
}

func (a *app) onLayoutSelect() {
	i := a.tvLayout.CurrentIndex()
	if i < 0 || i >= len(a.fields) {
		return
	}
	f := a.fields[i]
	a.edName.SetText(f.Name)
	a.edStart.SetText(strconv.Itoa(f.Start + 1))
	a.edLen.SetText(strconv.Itoa(f.Length))
	a.cbType.SetCurrentIndex(typeIndex(f.TypeCode()))
	if f.Scale != 0 {
		a.edScale.SetText(strconv.Itoa(f.Scale))
	} else {
		a.edScale.SetText("")
	}
}

func (a *app) readEditFields() (engine.Field, bool) {
	start, err1 := strconv.Atoi(strings.TrimSpace(a.edStart.Text()))
	length, err2 := strconv.Atoi(strings.TrimSpace(a.edLen.Text()))
	if err1 != nil || err2 != nil || start < 1 || length < 1 {
		a.fail("フィールド編集", "開始位置(1始まり)とバイト数は正の整数で入力してください")
		return engine.Field{}, false
	}
	code := typeLabels[max(a.cbType.CurrentIndex(), 0)].code
	ftype, signed, err := engine.NormalizeType(code)
	if err != nil {
		a.fail("フィールド編集", err.Error())
		return engine.Field{}, false
	}
	scale := 0
	if s := strings.TrimSpace(a.edScale.Text()); s != "" {
		if scale, err = strconv.Atoi(s); err != nil || scale < 0 {
			a.fail("フィールド編集", "小数桁が不正です: "+s)
			return engine.Field{}, false
		}
	}
	name := strings.TrimSpace(a.edName.Text())
	if name == "" {
		name = fmt.Sprintf("F%03d", len(a.fields)+1)
	}
	return engine.NewField(name, start-1, length, ftype, scale, signed), true
}

func (a *app) addField() {
	// 開始位置が空なら直前フィールドの終端+1を補完
	if strings.TrimSpace(a.edStart.Text()) == "" {
		a.edStart.SetText(strconv.Itoa(engine.RecordLengthOf(a.fields) + 1))
	}
	f, ok := a.readEditFields()
	if !ok {
		return
	}
	a.fields = append(a.fields, f)
	a.refreshLayoutTable()
	// 次の追加に備えて開始位置を進める
	a.edName.SetText("")
	a.edStart.SetText(strconv.Itoa(f.End() + 1))
	a.setStatus(fmt.Sprintf("フィールド %s を追加しました (計%d件)", f.Name, len(a.fields)))
}

func (a *app) updateField() {
	i := a.tvLayout.CurrentIndex()
	if i < 0 || i >= len(a.fields) {
		a.info("更新", "更新する行を一覧から選択してください")
		return
	}
	f, ok := a.readEditFields()
	if !ok {
		return
	}
	a.fields[i] = f
	a.refreshLayoutTable()
	a.tvLayout.SetCurrentIndex(i)
	a.setStatus(fmt.Sprintf("フィールド %s を更新しました", f.Name))
}

func (a *app) deleteField() {
	i := a.tvLayout.CurrentIndex()
	if i < 0 || i >= len(a.fields) {
		a.info("削除", "削除する行を一覧から選択してください")
		return
	}
	name := a.fields[i].Name
	a.fields = append(a.fields[:i:i], a.fields[i+1:]...)
	a.refreshLayoutTable()
	a.setStatus(fmt.Sprintf("フィールド %s を削除しました", name))
}

func (a *app) clearLayout() {
	if len(a.fields) > 0 && !a.confirm("全クリア", "レイアウトをすべて削除しますか?") {
		return
	}
	a.fields = nil
	a.refreshLayoutTable()
}

func (a *app) loadCopybook() {
	dlg := walk.FileDialog{
		Title:  "コピー句ファイルを選択",
		Filter: "コピー句 (*.cpy;*.cbl;*.cob;*.txt)|*.cpy;*.cbl;*.cob;*.txt|すべて (*.*)|*.*",
	}
	if ok, err := dlg.ShowOpen(a.mw); err != nil || !ok {
		return
	}
	nodes, err := engine.ParseCopybook(dlg.FilePath, "")
	if err != nil {
		a.fail("コピー句読込", fmt.Sprintf("コピー句を解析できませんでした:\n%v", err))
		return
	}
	fields, total := engine.CopybookToFields(nodes)
	if len(fields) == 0 {
		a.fail("コピー句読込", "コピー句を解析できませんでした:\nデータ項目が見つかりませんでした")
		return
	}
	a.fields = fields
	a.edRecLen.SetText(strconv.Itoa(total))
	a.refreshLayoutTable()
	a.updateFileInfo()
	a.setStatus(fmt.Sprintf("コピー句を読込みました: %d項目 / レコード長 %d バイト", len(fields), total))
	if a.edInput.Text() != "" {
		a.refreshPreview("")
	}
}

func (a *app) loadLayout() {
	dlg := walk.FileDialog{
		Title:  "レイアウトCSVを選択",
		Filter: "CSV (*.csv)|*.csv|すべて (*.*)|*.*",
	}
	if ok, err := dlg.ShowOpen(a.mw); err != nil || !ok {
		return
	}
	fields, err := engine.LoadLayoutCSV(dlg.FilePath)
	if err != nil {
		a.fail("レイアウト読込", fmt.Sprintf("レイアウトCSVを読めませんでした:\n%v", err))
		return
	}
	a.fields = fields
	if strings.TrimSpace(a.edRecLen.Text()) == "" {
		a.edRecLen.SetText(strconv.Itoa(engine.RecordLengthOf(fields)))
	}
	a.refreshLayoutTable()
	a.updateFileInfo()
	a.setStatus(fmt.Sprintf("レイアウトCSVを読込みました: %d項目", len(fields)))
	if a.edInput.Text() != "" {
		a.refreshPreview("")
	}
}

// withExt は拡張子のないパスに既定の拡張子を補う。
func withExt(path, ext string) string {
	if filepath.Ext(path) == "" {
		return path + ext
	}
	return path
}

func (a *app) saveLayout() {
	if len(a.fields) == 0 {
		a.info("CSV保存", "保存するレイアウトがありません")
		return
	}
	dlg := walk.FileDialog{Title: "レイアウトCSVを保存", Filter: "CSV (*.csv)|*.csv"}
	if ok, err := dlg.ShowSave(a.mw); err != nil || !ok {
		return
	}
	path := withExt(dlg.FilePath, ".csv")
	if err := engine.SaveLayoutCSV(a.fields, path); err != nil {
		a.fail("CSV保存", fmt.Sprintf("レイアウトを保存できませんでした:\n%v", err))
		return
	}
	a.setStatus("レイアウトを保存しました: " + path)
}

func (a *app) guess() {
	path, ok := a.needInput()
	if !ok {
		return
	}
	rl, ok := a.getRecLen(true)
	if !ok {
		// レコード長が未設定なら先に検出し、その完了後に改めて推測する
		if a.runBG("detect", detectWorker(path), "レコード長を検出しています...", a.onDetect) {
			a.guessAfterDet = true
		}
		return
	}
	a.runBG("guess", func() (any, error) {
		records, err := engine.SampleRecords(path, rl, 1000)
		if err != nil || len(records) == 0 {
			return []engine.Field(nil), err
		}
		return engine.GuessLayout(records), nil
	}, "レイアウトを推測しています...", a.onGuess)
}

func (a *app) onGuess(result any, err error, elapsed float64) {
	if err != nil {
		a.setStatus(fmt.Sprintf("推測に失敗しました: %v", err))
		a.fail("推測", fmt.Sprintf("レイアウトを推測できませんでした:\n%v", err))
		return
	}
	fields := result.([]engine.Field)
	if fields == nil {
		rl, _ := a.getRecLen(true)
		a.warn("推測", fmt.Sprintf("ファイルが1レコード(%dバイト)未満です", rl))
		return
	}
	a.fields = fields
	a.refreshLayoutTable()
	// プレビュー完了時のメッセージで推測の注意書きが消えないよう明示的に渡す
	a.refreshPreview(fmt.Sprintf("レイアウトを推測しました: %d項目 (%.2f秒) "+
		"※推測結果です。プレビューを確認し、必要に応じて修正してください "+
		"(パックの小数桁・バイナリ項目は判別できません)", len(fields), elapsed))
}

// ------------------------------------------------------------ プレビュー

type previewResult struct {
	fields []engine.Field
	rows   [][]string
	nerr   int64
}

// refreshPreview は status を渡すと、完了時のステータス表示をそれで上書きする。
func (a *app) refreshPreview(status string) {
	path, ok := a.needInput()
	if !ok {
		return
	}
	if len(a.fields) == 0 {
		a.info("プレビュー", "レイアウトがありません")
		return
	}
	rl, ok := a.getRecLen(false)
	if !ok {
		return
	}
	if over := engine.FieldsOverRecord(a.fields, rl); len(over) > 0 {
		a.fail("プレビュー", fmt.Sprintf("レコード長 %d を超えるフィールドがあります: %s", rl, engine.DescribeOver(over)))
		return
	}

	fields := append([]engine.Field(nil), a.fields...)
	keepZeros := a.chkKeepZeros.Checked()
	a.previewStatus = status
	a.runBG("preview", func() (any, error) {
		rows, nerr, err := engine.PreviewRows(path, rl, fields, keepZeros, previewRecords)
		return previewResult{fields, rows, nerr}, err
	}, "プレビューを作成しています...", a.onPreview)
}

// previewColumns は列構成が変わったときだけ列を組み直す。
func (a *app) previewColumns(fields []engine.Field) error {
	var sig strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&sig, "%q/%s/%d/%d;", f.Name, f.Type, f.Length, f.Scale)
	}
	if sig.String() == a.prevColsSig {
		return nil
	}
	a.previewModel.setRows(nil)
	cols := a.tvPreview.Columns()
	if err := cols.Clear(); err != nil {
		return err
	}
	for _, f := range fields {
		c := walk.NewTableViewColumn()
		if err := c.SetTitle(f.Name); err != nil {
			return err
		}
		if err := c.SetWidth(max(60, min(240, max(len([]rune(f.Name)), engine.DisplayWidth(f))*9))); err != nil {
			return err
		}
		if f.IsNumeric() {
			if err := c.SetAlignment(walk.AlignFar); err != nil {
				return err
			}
		}
		if err := cols.Add(c); err != nil {
			return err
		}
	}
	a.prevColsSig = sig.String()
	return nil
}

func (a *app) onPreview(result any, err error, elapsed float64) {
	if err != nil {
		a.setStatus(fmt.Sprintf("プレビューに失敗しました: %v", err))
		a.fail("プレビュー", fmt.Sprintf("プレビューを作成できませんでした:\n%v", err))
		return
	}
	r := result.(previewResult)
	if err := a.previewColumns(r.fields); err != nil {
		a.fail("プレビュー", fmt.Sprintf("プレビューを作成できませんでした:\n%v", err))
		return
	}
	a.previewModel.setRows(r.rows)
	info := fmt.Sprintf("%d 件表示 / %.2f秒", len(r.rows), elapsed)
	if r.nerr != 0 {
		info += fmt.Sprintf(" / 変換エラー %d 件 (#ERR表示) — レイアウトを確認してください", r.nerr)
	}
	a.lblPrevInfo.SetText(info)
	if a.previewStatus != "" {
		a.setStatus(a.previewStatus)
	} else {
		a.setStatus(fmt.Sprintf("プレビューを更新しました (%s)", info))
	}
}

func (a *app) refreshDump() {
	path := a.edInput.Text()
	if path == "" {
		return
	}
	rl, _ := a.getRecLen(true)
	a.runBG("dump", func() (any, error) {
		// 先頭 dumpRecords 件 (レコード長未確定なら dumpBytes) だけダンプする
		n := dumpBytes
		if rl > 0 {
			n = rl * dumpRecords
		}
		data, err := engine.ReadHead(path, n)
		if err != nil {
			return nil, err
		}
		return strings.ReplaceAll(engine.HexDump(data, rl, dumpRecords), "\n", "\r\n"), nil
	}, "16進ダンプを作成しています...", a.onDump)
}

func (a *app) onDump(text any, err error, _ float64) {
	if err != nil {
		a.setStatus(fmt.Sprintf("16進ダンプを作成できませんでした: %v", err))
		return
	}
	a.teDump.SetText(text.(string))
}

// ------------------------------------------------------------ 変換

func (a *app) pickOutput() {
	ext := ".txt"
	if a.rbCSV.Checked() {
		ext = ".csv"
	}
	dlg := walk.FileDialog{
		Title:  "出力ファイル",
		Filter: "CSV (*.csv)|*.csv|テキスト (*.txt)|*.txt|すべて (*.*)|*.*",
	}
	if ok, err := dlg.ShowSave(a.mw); err == nil && ok {
		a.edOutput.SetText(withExt(dlg.FilePath, ext))
	}
}

func (a *app) convert() {
	if a.converting {
		return
	}
	path, ok := a.needInput()
	if !ok {
		return
	}
	if len(a.fields) == 0 {
		a.warn("変換", "レイアウトを指定してください")
		return
	}
	rl, ok := a.getRecLen(false)
	if !ok {
		return
	}
	if over := engine.FieldsOverRecord(a.fields, rl); len(over) > 0 {
		a.fail("変換", fmt.Sprintf("レコード長 %d を超えるフィールドがあります: %s", rl, engine.DescribeOver(over)))
		return
	}
	out := strings.TrimSpace(a.edOutput.Text())
	if out == "" {
		a.warn("変換", "出力ファイルを指定してください")
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		a.fail("変換", err.Error())
		return
	}
	if rem := st.Size() % int64(rl); rem != 0 {
		msg := fmt.Sprintf("ファイルサイズ %s がレコード長 %d の倍数ではありません (末尾 %d バイトは無視されます)。\n続行しますか?",
			engine.Comma(st.Size()), rl, rem)
		if !a.confirm("確認", msg) {
			return
		}
	}
	if engine.SamePath(out, path) {
		a.fail("変換", "出力先が入力ファイルと同じです")
		return
	}

	a.converting = true
	a.btnConvert.SetEnabled(false)
	a.progress.SetValue(0)
	a.setStatus("変換中...")
	isCSV := a.rbCSV.Checked()
	opts := engine.ConvertOptions{
		Input:        path,
		Output:       out,
		Fields:       append([]engine.Field(nil), a.fields...),
		RecordLength: rl,
		CSV:          isCSV,
		ANSI:         a.rbANSI.Checked(),
		Delimiter:    " ",
		Header:       isCSV,
		KeepZeros:    a.chkKeepZeros.Checked(),
		Progress: func(fraction float64) {
			a.mw.Synchronize(func() { a.progress.SetValue(int(fraction * progressMax)) })
		},
	}
	go func() {
		var res engine.ConvertResult
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("内部エラー: %v", r)
				}
			}()
			res, err = engine.Convert(opts)
		}()
		a.mw.Synchronize(func() { a.onConverted(res, err, out) })
	}()
}

func (a *app) onConverted(res engine.ConvertResult, err error, out string) {
	a.converting = false
	a.btnConvert.SetEnabled(true)
	if err != nil {
		a.setStatus(fmt.Sprintf("エラー: %v", err))
		a.fail("変換エラー", err.Error())
		return
	}
	a.progress.SetValue(progressMax)
	nrec, nerr := engine.Comma(res.Records), engine.Comma(res.Errors.Count)
	status := fmt.Sprintf("完了: %s レコードを変換しました → %s", nrec, out)
	if res.Errors.Count != 0 {
		a.setStatus(fmt.Sprintf("%s (変換エラー %s 件 — #ERR表示)", status, nerr))
		a.warn("変換完了(警告あり)", fmt.Sprintf("%s レコードを変換しましたが、変換エラーが %s 件ありました。\n"+
			"出力中の #ERR:<16進> を確認し、レイアウトを見直してください。", nrec, nerr))
		return
	}
	a.setStatus(status)
	a.info("変換完了", fmt.Sprintf("%s レコードを変換しました。\n%s", nrec, out))
}

func main() {
	a := &app{
		layoutModel:  new(rowModel),
		previewModel: new(rowModel),
		inputSize:    -1,
		bgRunning:    map[string]bool{},
	}
	if err := a.build(); err != nil {
		walk.MsgBox(nil, "hostparse", fmt.Sprintf("ウィンドウを作成できませんでした:\n%v", err),
			walk.MsgBoxOK|walk.MsgBoxIconError)
		os.Exit(1)
	}
	a.rbCSV.SetChecked(true)
	a.rbUTF8.SetChecked(true)
	a.teDump.SetMaxLength(0) // 既定の 32KB を超えるダンプも表示できるよう上限を外す
	a.mw.Run()
}
