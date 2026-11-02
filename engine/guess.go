package engine

import (
	"fmt"
	"math"
	"os"
	"sort"
)

// byteClass は 0:空白 1:数字 2:半角カナ 3:ASCII印字 4:SJIS先行バイト候補 5:その他(バイナリ)
func byteClass(b byte) int {
	switch {
	case b == 0x20:
		return 0
	case b >= 0x30 && b <= 0x39:
		return 1
	case b >= 0xA1 && b <= 0xDF:
		return 2
	case b >= 0x21 && b <= 0x7E:
		return 3
	case isLead(b):
		return 4
	}
	return 5
}

func isLead(b byte) bool { return b >= 0x81 && b <= 0x9F || b >= 0xE0 && b <= 0xFC }

func isTrailByte(b byte) bool { return b >= 0x40 && b <= 0x7E || b >= 0x80 && b <= 0xFC }

const (
	// DefaultMaxRecordLength はレコード長の自動検出で考慮する上限。
	DefaultMaxRecordLength = 65536

	detectSample      = 16      // レコード長判定に使うレコード数
	detectSampleBytes = 1 << 20 // 先頭何バイトを読むか (1MB)
)

// classBits は「そのバイトがクラス c か空白か」を c ごとの 1 ビットで持つ表。
// 列の全レコードを AND して 0 でなければ、その列は均質。
var classBits = func() (t [256]byte) {
	for b := range t {
		c := byteClass(byte(b))
		for k := 1; k <= 5; k++ {
			if c == k || c == 0 {
				t[b] |= 1 << (k - 1)
			}
		}
	}
	return
}()

func divisorsUpTo(size, maxLen int64) []int {
	hi := min(size, maxLen)
	seen := map[int64]bool{}
	for d := int64(1); d*d <= size; d++ {
		if size%d != 0 {
			continue
		}
		for _, x := range []int64{d, size / d} {
			if x >= 2 && x <= hi {
				seen[x] = true
			}
		}
	}
	ds := make([]int, 0, len(seen))
	for x := range seen {
		ds = append(ds, int(x))
	}
	sort.Ints(ds)
	return ds
}

// Candidate はレコード長の候補とスコア。
type Candidate struct {
	RecordLength int
	Score        float64
}

// DetectRecordLength はファイルサイズの約数を候補に、列の均質さでレコード長を
// 推定する。検出できないときは 0 を返す。
func DetectRecordLength(path string, maxLen int) (int, []Candidate, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, nil, err
	}
	cands := divisorsUpTo(st.Size(), int64(maxLen))
	if len(cands) == 0 {
		return 0, nil, nil
	}
	// 最長候補 × detectSample 件ぶん読めば判定には十分。読込量を固定し、
	// ファイルサイズによらず一定時間で終わるようにする。
	head, err := ReadHead(path, int(min(st.Size(), int64(max(detectSampleBytes, maxLen*detectSample)))))
	if err != nil {
		return 0, nil, err
	}

	var results []Candidate
	for _, rl := range cands {
		avail := len(head) / rl
		if avail < 4 {
			continue // サンプル不足の候補は信頼できない
		}
		nrec := min(avail, detectSample)
		uniform := 0
		for col := 0; col < rl; col++ {
			bits := byte(0xFF)
			for r := 0; r < nrec && bits != 0; r++ {
				bits &= classBits[head[r*rl+col]]
			}
			if bits != 0 {
				uniform++
			}
		}
		// サンプル数が少ない候補は偶然揃いやすいためペナルティを課す
		score := float64(uniform)/float64(rl) - 0.03*float64(detectSample-nrec)
		results = append(results, Candidate{rl, score})
	}
	if len(results) == 0 {
		return 0, nil, nil
	}
	best := math.Inf(-1)
	for _, c := range results {
		best = max(best, c.Score)
	}
	// 最高スコアの95%以上のうち最小のレコード長を採用 (倍数を避ける)
	chosen := 0
	for _, c := range results {
		if c.Score >= best*0.95 && (chosen == 0 || c.RecordLength < chosen) {
			chosen = c.RecordLength
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	return chosen, results[:min(5, len(results))], nil
}

// FormatCandidates は候補を「46(100%), 92(100%)」形式で列挙する。
func FormatCandidates(cands []Candidate) string {
	s := ""
	for i, c := range cands {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%d(%.0f%%)", c.RecordLength, c.Score*100)
	}
	return s
}

// GuessLayout はサンプルレコード群からレイアウトを推測する。
func GuessLayout(records [][]byte) []Field {
	rl := len(records[0])
	var fields []Field
	col, fno := 0, 1

	colOK := func(i int, pred func(byte) bool) bool {
		for _, r := range records {
			if !pred(r[i]) {
				return false
			}
		}
		return true
	}
	anyCol := func(i int, pred func(byte) bool) bool {
		for _, r := range records {
			if pred(r[i]) {
				return true
			}
		}
		return false
	}
	pairOK := func(i int) bool {
		if i+1 >= rl {
			return false
		}
		found := false
		for _, r := range records {
			a, b := r[i], r[i+1]
			if a == 0x20 && b == 0x20 {
				continue
			}
			if isLead(a) && isTrailByte(b) {
				found = true
				continue
			}
			return false
		}
		return found
	}
	emit := func(start, length int, ftype string) {
		fields = append(fields, NewField(fmt.Sprintf("F%03d", fno), start, length, ftype, 0, false))
		fno++
	}

	isDigit := func(b byte) bool { return b >= 0x30 && b <= 0x39 }
	isKana := func(b byte) bool { return b >= 0xA1 && b <= 0xDF }
	isKanaOrSpace := func(b byte) bool { return isKana(b) || b == 0x20 }
	isPrint := func(b byte) bool { return b >= 0x20 && b <= 0x7E }
	nibDigit := func(b byte) bool { return b>>4 <= 9 && b&0xF <= 9 }
	signByte := func(b byte) bool { return b>>4 <= 9 && b&0xF >= 0xA }
	// テキスト系の領域が始まるなら true (バイナリ領域の打切り判定)
	textStarts := func(i int) bool {
		return i < rl && (pairOK(i) || colOK(i, isPrint) || colOK(i, isKanaOrSpace))
	}

	for col < rl {
		// 全角 (SJIS 2バイトペアの連続)
		if pairOK(col) {
			s := col
			for col+1 < rl && pairOK(col) {
				col += 2
			}
			emit(s, col-s, "N")
			continue
		}
		// ゾーン数値 (数字のみ ※空白のみの列は含めない)
		if colOK(col, isDigit) {
			s := col
			for col < rl && colOK(col, isDigit) {
				col++
			}
			emit(s, col-s, "9")
			continue
		}
		// 半角カナ (カナ or 空白)
		if colOK(col, isKanaOrSpace) && anyCol(col, isKana) {
			s := col
			for col < rl && colOK(col, isKanaOrSpace) {
				col++
			}
			emit(s, col-s, "K")
			continue
		}
		// 半角文字 (ASCII印字+空白)
		if colOK(col, isPrint) {
			s := col
			for col < rl && colOK(col, isPrint) && !pairOK(col) && !colOK(col, isDigit) {
				col++
			}
			emit(s, col-s, "X")
			continue
		}
		// バイナリ領域 → パック候補を探す
		s := col
		for col < rl {
			// col から始まるパックを探す (最大10バイト = 19桁)
			end := -1
			for j := col; j < min(col+10, rl); j++ {
				if colOK(j, signByte) {
					end = j
					break
				}
				if !colOK(j, nibDigit) {
					break
				}
			}
			if end >= 0 {
				if col > s {
					emit(s, col-s, "X") // パック前の未分類部分
				}
				emit(col, end-col+1, "P")
				col = end + 1
				s = col
				// 続けてバイナリ領域か確認
				if textStarts(col) {
					break
				}
				continue
			}
			// パックでない未分類バイト
			col++
			if textStarts(col) {
				break
			}
		}
		if col > s {
			emit(s, col-s, "X")
		}
	}
	return fields
}
