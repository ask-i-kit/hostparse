#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
hostparse.py — ホスト固定長ファイル パースツール

SJIS・改行なしの固定長レコードファイルを、コピー句 / 手動レイアウト / 推測 に
もとづいてパースし、テキストまたはCSVで出力する。パック項目(COMP-3)対応。

使用例:
    # コピー句からパースして CSV (UTF-8) 出力
    python hostparse.py data.dat -c copybook.cpy -o out.csv

    # 手動レイアウト(CSV)でテキスト(ANSI=CP932)出力
    python hostparse.py data.dat -l layout.csv --format text --encoding ansi -o out.txt

    # コマンドラインでフィールドを直接指定 (名前,開始位置(1始まり),バイト数,形式[,小数桁])
    python hostparse.py data.dat --field "社員番号,1,5,9" --field "金額,16,5,P,2" -o out.csv

    # レイアウトを推測 (レコード長も自動検出) してレイアウト案を保存
    python hostparse.py data.dat --guess

形式コード:
    X  … 半角文字 (英数記号)        K  … 半角カナ
    N  … 全角文字                   9  … ゾーン数値 (S9 で符号付き)
    P  … パック10進 (COMP-3)        B  … バイナリ (COMP)
"""

import argparse
import csv
import io
import os
import re
import sys
from dataclasses import dataclass, field as dc_field

INPUT_CODEC = "cp932"   # 入力ファイルの文字コード (SJIS)

# ---------------------------------------------------------------------------
# フィールド定義
# ---------------------------------------------------------------------------

@dataclass
class Field:
    name: str
    start: int          # 0始まりバイト位置
    length: int         # バイト数
    ftype: str          # X / K / N / 9 / P / B
    scale: int = 0      # 小数桁
    signed: bool = False
    digits: int = 0     # 数値の桁数 (表示幅計算用)

    def __post_init__(self):
        if self.digits == 0:
            if self.ftype == "P":
                self.digits = self.length * 2 - 1
            elif self.ftype in ("9", "B"):
                self.digits = self.length


TYPE_ALIASES = {
    "X": "X", "半角": "X", "文字": "X", "CHAR": "X", "A": "X",
    "K": "K", "カナ": "K", "半角カナ": "K", "KANA": "K",
    "N": "N", "全角": "N", "漢字": "N", "ZENKAKU": "N",
    "9": "9", "数値": "9", "ゾーン": "9", "ZONE": "9", "ZD": "9",
    "S9": "S9", "符号付数値": "S9", "符号付き数値": "S9",
    "P": "P", "パック": "P", "PACK": "P", "COMP-3": "P", "PD": "P",
    "SP": "P", "S9P": "P",
    "B": "B", "バイナリ": "B", "BINARY": "B", "COMP": "B",
}


def normalize_type(t: str):
    """形式文字列 -> (ftype, signed)"""
    key = t.strip().upper() if t.strip().isascii() else t.strip()
    if key not in TYPE_ALIASES:
        raise ValueError(f"不明な形式です: {t!r} (使用可能: X,K,N,9,S9,P,B / 半角,カナ,全角,数値,パック,バイナリ)")
    norm = TYPE_ALIASES[key]
    if norm == "S9":
        return "9", True
    signed = key in ("SP", "S9P")
    return norm, signed


# ---------------------------------------------------------------------------
# コピー句 (COBOL COPY句) パーサ
# ---------------------------------------------------------------------------

@dataclass
class CopyItem:
    level: int
    name: str
    pic: str = ""
    usage: str = "DISPLAY"
    occurs: int = 0
    redefines: str = ""
    children: list = dc_field(default_factory=list)


def _read_text_any(path: str, preferred: str = None) -> str:
    with open(path, "rb") as f:
        raw = f.read()
    encodings = [preferred] if preferred else []
    encodings += ["utf-8-sig", "cp932", "utf-8"]
    for enc in encodings:
        if not enc:
            continue
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    return raw.decode("cp932", errors="replace")


def _copybook_statements(text: str):
    """コメント・行番号領域を除去し、ピリオド区切りの文へ分解する。"""
    bodies = []
    for line in text.splitlines():
        line = line.rstrip()
        if not line.strip():
            continue
        # 固定形式 (1-6桁: 行番号, 7桁目: 標識, 73桁目以降: 識別領域) の判定
        head = line[:6]
        if len(line) > 6 and (head.strip() == "" or head.strip().isdigit()):
            indicator = line[6]
            if indicator in "*/":
                continue
            body = line[7:72]
        else:
            if line.lstrip().startswith("*"):
                continue
            body = line
        bodies.append(body)
    src = " ".join(bodies)

    # 引用符内を保護しつつピリオドで分割
    stmts, buf, quote = [], [], None
    i = 0
    while i < len(src):
        ch = src[i]
        if quote:
            buf.append(ch)
            if ch == quote:
                quote = None
        elif ch in ("'", '"'):
            quote = ch
            buf.append(ch)
        elif ch == "." and (i + 1 >= len(src) or src[i + 1] in " \t"):
            s = "".join(buf).strip()
            if s:
                stmts.append(s)
            buf = []
        else:
            buf.append(ch)
        i += 1
    tail = "".join(buf).strip()
    if tail:
        stmts.append(tail)
    return stmts


_PIC_RE = re.compile(r"\bPIC(?:TURE)?(?:\s+IS)?\s+([\w()VvSsXxNnZz9.,*+\-/$]+)", re.I)
_USAGE_RE = re.compile(
    r"\b(COMP-3|COMPUTATIONAL-3|PACKED-DECIMAL|COMP-5|COMPUTATIONAL-5|COMP-4|"
    r"COMPUTATIONAL-4|COMP-1|COMP-2|COMP|COMPUTATIONAL|BINARY|DISPLAY)\b", re.I)
_OCCURS_RE = re.compile(r"\bOCCURS\s+(?:\d+\s+TO\s+)?(\d+)", re.I)
_REDEF_RE = re.compile(r"\bREDEFINES\s+([\w\-]+)", re.I)
_STMT_RE = re.compile(r"^(\d{1,2})\s+([\w\-]+)?\s*(.*)$", re.S)


def parse_copybook(path: str, encoding: str = None) -> list:
    """コピー句を解析して CopyItem のツリー(トップレベルのリスト)を返す。"""
    text = _read_text_any(path, encoding)
    items = []
    for stmt in _copybook_statements(text):
        m = _STMT_RE.match(stmt)
        if not m:
            continue
        level = int(m.group(1))
        if level in (66, 77, 88):
            if level == 77:
                pass  # 77 は独立項目として扱う
            else:
                continue
        name = (m.group(2) or "FILLER").upper()
        rest = m.group(3) or ""
        if name in ("PIC", "PICTURE", "REDEFINES", "OCCURS", "VALUE"):
            rest = name + " " + rest
            name = "FILLER"
        item = CopyItem(level=level, name=name)
        pm = _PIC_RE.search(rest)
        if pm:
            item.pic = pm.group(1).upper()
        um = _USAGE_RE.search(rest)
        if um:
            u = um.group(1).upper()
            if u in ("COMP-3", "COMPUTATIONAL-3", "PACKED-DECIMAL"):
                item.usage = "PACK"
            elif u in ("COMP", "COMPUTATIONAL", "COMP-4", "COMPUTATIONAL-4",
                       "COMP-5", "COMPUTATIONAL-5", "BINARY"):
                item.usage = "BINARY"
            else:
                item.usage = "DISPLAY"
        om = _OCCURS_RE.search(rest)
        if om:
            item.occurs = int(om.group(1))
        rm = _REDEF_RE.search(rest)
        if rm:
            item.redefines = rm.group(1).upper()
        items.append(item)

    if not items:
        raise ValueError(f"コピー句からデータ項目を読み取れませんでした: {path}")

    # レベル番号でツリー構築
    root = CopyItem(level=0, name="__ROOT__")
    stack = [root]
    for it in items:
        while len(stack) > 1 and stack[-1].level >= it.level:
            stack.pop()
        stack[-1].children.append(it)
        stack.append(it)
    return root.children


def _expand_pic(pic: str) -> str:
    """X(3)9(2) -> XXX99 のように展開する。"""
    def rep(m):
        return m.group(1) * int(m.group(2))
    return re.sub(r"([XANZ9*BP])\((\d+)\)", rep, pic, flags=0)


def pic_info(pic: str, usage: str):
    """PIC文字列 -> (バイト数, ftype, digits, scale, signed)"""
    p = _expand_pic(pic)
    signed = p.startswith("S")
    if signed:
        p = p[1:]
    if "N" in p:
        n = p.count("N")
        return n * 2, "N", 0, 0, False
    if "X" in p or "A" in p:
        n = sum(1 for c in p if c in "XA9")
        return n, "X", 0, 0, False
    # 数値
    intpart, _, decpart = p.partition("V")
    digits = sum(1 for c in p if c in "9Z*")
    scale = sum(1 for c in decpart if c in "9Z*")
    edit_chars = sum(1 for c in p if c in ".,+-/$B0")
    if usage == "PACK":
        return digits // 2 + 1, "P", digits, scale, signed
    if usage == "BINARY":
        size = 2 if digits <= 4 else (4 if digits <= 9 else 8)
        return size, "B", digits, scale, signed
    # DISPLAY (ゾーン数値 / 編集項目)
    size = digits + edit_chars
    if edit_chars:
        return size, "X", 0, 0, False  # 編集項目は文字として扱う
    return size, "9", digits, scale, signed


def copybook_to_fields(nodes: list) -> list:
    """コピー句ツリーを平坦化して Field のリストを返す。"""
    fields = []

    def walk(children, start, suffix):
        starts = {}
        cur = start
        maxend = start
        for c in children:
            if c.redefines and c.redefines in starts:
                pos = starts[c.redefines]
                advance = False
            else:
                pos = cur
                advance = True
            starts[c.name] = pos
            n = c.occurs if c.occurs else 1
            p = pos
            for i in range(n):
                sfx = suffix + (f"({i + 1})" if c.occurs else "")
                if c.children:
                    end = walk(c.children, p, sfx)
                elif c.pic:
                    size, ftype, digits, scale, signed = pic_info(c.pic, c.usage)
                    if c.name != "FILLER":
                        fields.append(Field(
                            name=c.name + sfx, start=p, length=size,
                            ftype=ftype, scale=scale, signed=signed,
                            digits=digits or size))
                    end = p + size
                else:
                    end = p  # PIC なしの単独項目 (サイズ0)
                p = end
            if advance:
                cur = p
            maxend = max(maxend, p)
        return maxend

    total = walk(nodes, 0, "")
    return fields, total


# ---------------------------------------------------------------------------
# 手動レイアウト (CSVファイル / --field)
# ---------------------------------------------------------------------------

_LAYOUT_KEYS = {
    "name": "name", "名前": "name", "項目名": "name", "項目": "name",
    "start": "start", "開始": "start", "開始位置": "start", "位置": "start",
    "length": "length", "バイト数": "length", "長さ": "length", "len": "length",
    "type": "type", "形式": "type", "タイプ": "type", "型": "type",
    "scale": "scale", "小数桁": "scale", "小数": "scale",
}


def load_layout_csv(path: str) -> list:
    text = _read_text_any(path)
    reader = csv.reader(text.splitlines())
    rows = [r for r in reader if r and any(x.strip() for x in r)]
    if not rows:
        raise ValueError(f"レイアウトファイルが空です: {path}")

    header = [c.strip().lower() if c.strip().isascii() else c.strip() for c in rows[0]]
    has_header = any(h in _LAYOUT_KEYS for h in header)
    if has_header:
        idx = {}
        for i, h in enumerate(header):
            if h in _LAYOUT_KEYS:
                idx[_LAYOUT_KEYS[h]] = i
        data_rows = rows[1:]
    else:
        idx = {"name": 0, "start": 1, "length": 2, "type": 3, "scale": 4}
        data_rows = rows

    for key in ("start", "length", "type"):
        if key not in idx:
            raise ValueError(f"レイアウトに列がありません: {key} (開始/バイト数/形式)")

    fields = []
    for ln, r in enumerate(data_rows, 2 if has_header else 1):
        try:
            name = r[idx["name"]].strip() if "name" in idx and idx["name"] < len(r) else ""
            start = int(r[idx["start"]].strip())
            length = int(r[idx["length"]].strip())
            ftype, signed = normalize_type(r[idx["type"]])
            scale = 0
            if "scale" in idx and idx["scale"] < len(r) and r[idx["scale"]].strip():
                scale = int(r[idx["scale"]].strip())
        except (ValueError, IndexError) as e:
            raise ValueError(f"レイアウト {path} の {ln} 行目を解釈できません: {r} ({e})")
        if start < 1:
            raise ValueError(f"開始位置は1始まりで指定してください ({ln}行目: {start})")
        fields.append(Field(name=name or f"F{ln:03d}", start=start - 1,
                            length=length, ftype=ftype, scale=scale, signed=signed))
    return fields


def parse_field_args(specs: list) -> list:
    """--field "名前,開始,バイト数,形式[,小数桁]" を解釈する。"""
    fields = []
    for i, spec in enumerate(specs, 1):
        parts = [p.strip() for p in spec.split(",")]
        if len(parts) < 4:
            raise ValueError(f"--field は 名前,開始,バイト数,形式[,小数桁] で指定してください: {spec!r}")
        name, start, length, ftype = parts[0], int(parts[1]), int(parts[2]), parts[3]
        scale = int(parts[4]) if len(parts) > 4 and parts[4] else 0
        ft, signed = normalize_type(ftype)
        fields.append(Field(name=name or f"F{i:03d}", start=start - 1,
                            length=length, ftype=ft, scale=scale, signed=signed))
    return fields


# ---------------------------------------------------------------------------
# 変換関数 (フィールドごとに事前生成 → 高速化)
# ---------------------------------------------------------------------------

# ゾーン数値の符号オーバーパンチ (EBCDIC由来 / ASCII系)
_OVERPUNCH_POS = {"{": "0", "A": "1", "B": "2", "C": "3", "D": "4",
                  "E": "5", "F": "6", "G": "7", "H": "8", "I": "9"}
_OVERPUNCH_NEG = {"}": "0", "J": "1", "K": "2", "L": "3", "M": "4",
                  "N": "5", "O": "6", "P": "7", "Q": "8", "R": "9"}

_PACK_SIGN_NEG = frozenset("db")
_PACK_SIGN_POS = frozenset("cfae")


class ErrorCounter:
    __slots__ = ("count", "first")

    def __init__(self):
        self.count = 0
        self.first = None

    def hit(self, name, value):
        self.count += 1
        if self.first is None:
            self.first = (name, value)


def _apply_scale(digits_str: str, scale: int, keep_zeros: bool) -> str:
    if scale:
        if len(digits_str) <= scale:
            digits_str = digits_str.rjust(scale + 1, "0")
        s = digits_str[:-scale] + "." + digits_str[-scale:]
    else:
        s = digits_str
    if not keep_zeros:
        s = s.lstrip("0") or "0"
        if s.startswith("."):
            s = "0" + s
    return s


def make_converter(f: Field, strip_text: bool, keep_zeros: bool, errors: ErrorCounter):
    """Field -> bytes を受け取り文字列を返す関数"""
    ftype, scale, signed, name = f.ftype, f.scale, f.signed, f.name

    if ftype in ("X", "K", "N"):
        if strip_text:
            def conv(b):
                return b.decode(INPUT_CODEC, "replace").rstrip()
        else:
            def conv(b):
                return b.decode(INPUT_CODEC, "replace")
        return conv

    if ftype == "9":
        pos_map, neg_map = _OVERPUNCH_POS, _OVERPUNCH_NEG

        def conv(b):
            # ファストパス: 純粋な数字のみ (大半のケース)
            if b.isdigit():
                if scale:
                    return _apply_scale(b.decode("ascii"), scale, keep_zeros)
                return b.decode("ascii") if keep_zeros else str(int(b))
            s = b.decode(INPUT_CODEC, "replace").strip()
            if not s:
                return ""
            neg = False
            if s[0] in "+-":
                neg = s[0] == "-"
                s = s[1:]
            elif s[-1] in "+-":
                neg = s[-1] == "-"
                s = s[:-1]
            elif s[-1] in pos_map:
                s = s[:-1] + pos_map[s[-1]]
            elif s[-1] in neg_map:
                s = s[:-1] + neg_map[s[-1]]
                neg = True
            if not s.isdigit():
                errors.hit(name, b.hex())
                return "#ERR:" + b.hex()
            v = _apply_scale(s, scale, keep_zeros)
            return ("-" + v) if neg and v.strip("0.") else v
        return conv

    if ftype == "P":
        sign_neg, sign_pos = _PACK_SIGN_NEG, _PACK_SIGN_POS

        def conv(b):
            h = b.hex()
            sign = h[-1]
            digits = h[:-1]
            if not digits.isdigit() or (sign not in sign_pos and sign not in sign_neg):
                if b.strip(b"\x00 ") == b"":
                    return ""  # 全ゼロ埋め/空白は空とみなす
                errors.hit(name, h)
                return "#ERR:" + h
            v = _apply_scale(digits, scale, keep_zeros)
            return ("-" + v) if sign in sign_neg and v.strip("0.") else v
        return conv

    if ftype == "B":
        def conv(b):
            n = int.from_bytes(b, "big", signed=signed)
            s = str(abs(n)) if n < 0 else str(n)
            if scale:
                s = _apply_scale(s.rjust(scale + 1, "0"), scale, keep_zeros)
            return ("-" + s) if n < 0 else s
        return conv

    raise ValueError(f"未対応の形式: {ftype}")


def display_width(f: Field) -> int:
    """テキスト出力時のフィールド表示幅"""
    if f.ftype in ("X", "K", "N"):
        return f.length
    if f.ftype == "9":
        return f.length + (1 if f.signed else 0) + (1 if f.scale else 0)
    if f.ftype == "P":
        return f.digits + 1 + (1 if f.scale else 0)
    if f.ftype == "B":
        return len(str(2 ** (8 * f.length))) + 1
    return f.length


# ---------------------------------------------------------------------------
# レイアウト推測
# ---------------------------------------------------------------------------

def _byte_class(b: int) -> int:
    """0:空白 1:数字 2:半角カナ 3:ASCII印字 4:SJIS先行バイト候補 5:その他(バイナリ)"""
    if b == 0x20:
        return 0
    if 0x30 <= b <= 0x39:
        return 1
    if 0xA1 <= b <= 0xDF:
        return 2
    if 0x21 <= b <= 0x7E:
        return 3
    if 0x81 <= b <= 0x9F or 0xE0 <= b <= 0xFC:
        return 4
    return 5


def _is_lead(b):
    return 0x81 <= b <= 0x9F or 0xE0 <= b <= 0xFC


def _is_trail(b):
    return 0x40 <= b <= 0x7E or 0x80 <= b <= 0xFC


def detect_record_length(path: str, max_len: int = 65536):
    """ファイルサイズの約数を候補に、列の均質さでレコード長を推定する。"""
    size = os.path.getsize(path)
    cands = [d for d in range(2, min(size, max_len) + 1) if size % d == 0]
    if not cands:
        return None, []
    results = []
    with open(path, "rb") as fp:
        head = fp.read(min(size, 4 * 1024 * 1024))
    SAMPLE = 16
    for rl in cands:
        avail = len(head) // rl
        if avail < 4:
            continue  # サンプル不足の候補は信頼できない
        nrec = min(avail, SAMPLE)
        recs = [head[i * rl:(i + 1) * rl] for i in range(nrec)]
        uniform = 0
        for col in range(rl):
            classes = {_byte_class(r[col]) for r in recs}
            classes.discard(0)  # 空白は無視
            if len(classes) <= 1:
                uniform += 1
        # サンプル数が少ない候補は偶然揃いやすいためペナルティを課す
        score = uniform / rl - 0.03 * (SAMPLE - nrec)
        results.append((rl, score))
    if not results:
        return None, []
    best_score = max(s for _, s in results)
    # 最高スコアの95%以上のうち最小のレコード長を採用 (倍数を避ける)
    good = [rl for rl, s in results if s >= best_score * 0.95]
    results.sort(key=lambda x: -x[1])
    return min(good), results[:5]


def guess_layout(records: list) -> list:
    """サンプルレコード群からレイアウトを推測する。"""
    rl = len(records[0])
    fields = []
    col = 0
    fno = 1

    def col_ok(i, pred):
        return all(pred(r[i]) for r in records)

    def any_col(i, pred):
        return any(pred(r[i]) for r in records)

    def pair_ok(i):
        if i + 1 >= rl:
            return False
        found = False
        for r in records:
            a, b = r[i], r[i + 1]
            if a == 0x20 and b == 0x20:
                continue
            if _is_lead(a) and _is_trail(b):
                found = True
                continue
            return False
        return found

    def emit(start, length, ftype, scale=0):
        nonlocal fno
        fields.append(Field(name=f"F{fno:03d}", start=start, length=length,
                            ftype=ftype, scale=scale))
        fno += 1

    while col < rl:
        # 全角 (SJIS 2バイトペアの連続)
        if pair_ok(col):
            s = col
            while col + 1 < rl and pair_ok(col):
                col += 2
            emit(s, col - s, "N")
            continue
        # ゾーン数値 (数字のみ ※空白のみの列は含めない)
        if col_ok(col, lambda b: 0x30 <= b <= 0x39):
            s = col
            while col < rl and col_ok(col, lambda b: 0x30 <= b <= 0x39):
                col += 1
            emit(s, col - s, "9")
            continue
        # 半角カナ (カナ or 空白)
        if col_ok(col, lambda b: 0xA1 <= b <= 0xDF or b == 0x20) and \
                any_col(col, lambda b: 0xA1 <= b <= 0xDF):
            s = col
            while col < rl and col_ok(col, lambda b: 0xA1 <= b <= 0xDF or b == 0x20):
                col += 1
            emit(s, col - s, "K")
            continue
        # 半角文字 (ASCII印字+空白)
        if col_ok(col, lambda b: 0x20 <= b <= 0x7E):
            s = col
            while col < rl and col_ok(col, lambda b: 0x20 <= b <= 0x7E) and \
                    not pair_ok(col) and \
                    not col_ok(col, lambda b: 0x30 <= b <= 0x39):
                col += 1
            emit(s, col - s, "X")
            continue
        # バイナリ領域 → パック候補を探す
        s = col
        while col < rl:
            # パック終端候補: 上位ニブル<=9, 下位ニブルが符号 (C/D/F/A/B/E)
            def nib_digit(i):
                return col_ok(i, lambda b: (b >> 4) <= 9 and (b & 0xF) <= 9)

            def sign_byte(i):
                return col_ok(i, lambda b: (b >> 4) <= 9 and (b & 0xF) >= 0xA)

            # col から始まるパックを探す (最大10バイト = 19桁)
            end = -1
            j = col
            while j < min(col + 10, rl):
                if sign_byte(j):
                    end = j
                    break
                if not nib_digit(j):
                    break
                j += 1
            if end >= 0:
                if col > s:
                    emit(s, col - s, "X")  # パック前の未分類部分
                emit(col, end - col + 1, "P")
                col = end + 1
                s = col
                # 続けてバイナリ領域か確認
                if col < rl and (pair_ok(col) or col_ok(col, lambda b: 0x20 <= b <= 0x7E)
                                 or col_ok(col, lambda b: 0xA1 <= b <= 0xDF or b == 0x20)):
                    break
                continue
            # パックでない未分類バイト
            col += 1
            if col < rl and (pair_ok(col) or col_ok(col, lambda b: 0x20 <= b <= 0x7E)
                             or col_ok(col, lambda b: 0xA1 <= b <= 0xDF or b == 0x20)):
                break
        if col > s:
            emit(s, col - s, "X")
    return fields


def save_layout_csv(fields: list, path: str):
    with open(path, "w", encoding="utf-8-sig", newline="") as fp:
        w = csv.writer(fp)
        w.writerow(["名前", "開始", "バイト数", "形式", "小数桁"])
        for f in fields:
            w.writerow([f.name, f.start + 1, f.length, f.ftype, f.scale or ""])


def format_layout(fields: list, record_length: int) -> str:
    type_names = {"X": "半角文字", "K": "半角カナ", "N": "全角文字",
                  "9": "ゾーン数値", "P": "パック", "B": "バイナリ"}
    lines = [f"{'名前':<20} {'開始':>5} {'バイト':>5} {'形式':<4} {'内容'}"]
    lines.append("-" * 60)
    for f in fields:
        desc = type_names.get(f.ftype, f.ftype)
        if f.signed:
            desc += "(符号付)"
        if f.scale:
            desc += f" 小数{f.scale}桁"
        if f.ftype in ("P", "B"):
            desc += f" {f.digits}桁"
        lines.append(f"{f.name:<20} {f.start + 1:>5} {f.length:>5} {f.ftype:<4} {desc}")
    lines.append("-" * 60)
    lines.append(f"レコード長: {record_length} バイト")
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# メイン処理
# ---------------------------------------------------------------------------

def process(args, fields, record_length):
    errors = ErrorCounter()
    is_csv = args.format == "csv"
    strip_text = is_csv
    convs = [(slice(f.start, f.start + f.length),
              make_converter(f, strip_text, args.keep_zeros, errors))
             for f in fields]
    widths = [display_width(f) for f in fields]
    numeric = [f.ftype in ("9", "P", "B") for f in fields]

    out_enc = "cp932" if args.encoding == "ansi" else "utf-8"
    if is_csv and out_enc == "utf-8":
        out_enc = "utf-8-sig"  # Excel対応のためBOM付き

    if args.output:
        if is_csv:
            out_fp = open(args.output, "w", encoding=out_enc, newline="", errors="replace")
        else:
            out_fp = open(args.output, "w", encoding=out_enc, errors="replace")
    else:
        out_fp = io.TextIOWrapper(sys.stdout.buffer, encoding=out_enc,
                                  newline="" if is_csv else None, errors="replace")

    file_size = os.path.getsize(args.input)
    if file_size % record_length != 0:
        print(f"警告: ファイルサイズ {file_size} がレコード長 {record_length} の"
              f"倍数ではありません (末尾 {file_size % record_length} バイトは無視されます)",
              file=sys.stderr)

    nrec = 0
    delim = args.delimiter
    try:
        writer = csv.writer(out_fp) if is_csv else None
        if is_csv and not args.no_header:
            writer.writerow([f.name for f in fields])
        elif not is_csv and args.header:
            out_fp.write(delim.join(
                (f.name[:w].ljust(w) if len(f.name) <= w else f.name)
                for f, w in zip(fields, widths)) + "\n")

        chunk_recs = max(1, (8 * 1024 * 1024) // record_length)
        chunk_size = chunk_recs * record_length
        limit = args.limit if args.limit else float("inf")
        skip_bytes = args.skip * record_length if args.skip else 0

        with open(args.input, "rb") as fp:
            if skip_bytes:
                fp.seek(skip_bytes)
            rows_buf = []
            while nrec < limit:
                chunk = fp.read(chunk_size)
                if len(chunk) < record_length:
                    break
                usable = (len(chunk) // record_length) * record_length
                for off in range(0, usable, record_length):
                    row = [fn(chunk[off + sl.start:off + sl.stop]) for sl, fn in convs]
                    if is_csv:
                        rows_buf.append(row)
                    else:
                        out_fp.write(delim.join(
                            (v.rjust(w) if num else v.ljust(w) if len(v) <= w else v)
                            for v, w, num in zip(row, widths, numeric)) + "\n")
                    nrec += 1
                    if nrec >= limit:
                        break
                if is_csv and rows_buf:
                    writer.writerows(rows_buf)
                    rows_buf = []
    finally:
        if args.output:
            out_fp.close()
        else:
            out_fp.flush()
            out_fp.detach()

    print(f"完了: {nrec:,} レコードを処理しました。", file=sys.stderr)
    if errors.count:
        name, val = errors.first
        print(f"警告: 変換エラー {errors.count:,} 件 (最初: 項目={name} 値=0x{val})。"
              f" 出力中は #ERR:<hex> と表示されています。レイアウトを確認してください。",
              file=sys.stderr)


def main(argv=None):
    ap = argparse.ArgumentParser(
        prog="hostparse",
        description="ホスト固定長ファイル(SJIS・改行なし)をパースしてテキスト/CSVに変換する。",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__.split("使用例:")[1] if "使用例:" in __doc__ else "")
    ap.add_argument("input", help="入力ファイル (SJIS・改行なしの固定長レコード)")
    src = ap.add_argument_group("レイアウト指定 (いずれか1つ)")
    src.add_argument("-c", "--copybook", help="コピー句ファイル (COBOL COPY句)")
    src.add_argument("-l", "--layout", help="レイアウトCSVファイル (名前,開始,バイト数,形式,小数桁)")
    src.add_argument("-f", "--field", action="append", default=[],
                     help='フィールド直接指定 "名前,開始,バイト数,形式[,小数桁]" (複数指定可)')
    src.add_argument("-g", "--guess", action="store_true",
                     help="レイアウトを推測する (レイアウト案をCSV保存)")
    out = ap.add_argument_group("出力")
    out.add_argument("-o", "--output", help="出力ファイル (省略時: 標準出力)")
    out.add_argument("--format", choices=["text", "csv"],
                     help="出力形式 (省略時: 出力ファイルの拡張子から判断, 既定 text)")
    out.add_argument("--encoding", choices=["utf-8", "ansi"], default="utf-8",
                     help="出力文字コード (ansi=CP932/Shift_JIS, 既定 utf-8)")
    out.add_argument("--delimiter", default=" ", help="text形式の項目区切り (既定: 半角空白)")
    out.add_argument("--header", action="store_true", help="text形式でも見出し行を出力する")
    out.add_argument("--no-header", action="store_true", help="csv形式で見出し行を出力しない")
    out.add_argument("--keep-zeros", action="store_true",
                     help="数値の先頭ゼロを残す (既定: 除去して 00123 -> 123)")
    opt = ap.add_argument_group("その他")
    opt.add_argument("-r", "--record-length", type=int,
                     help="レコード長(バイト)。コピー句指定時は省略可")
    opt.add_argument("--limit", type=int, help="先頭 N レコードのみ処理")
    opt.add_argument("--skip", type=int, default=0, help="先頭 N レコードを読み飛ばす")
    opt.add_argument("--show-layout", action="store_true",
                     help="レイアウトを表示して終了 (変換しない)")
    opt.add_argument("--save-layout", metavar="FILE",
                     help="解決したレイアウトをCSVで保存する")
    opt.add_argument("--copybook-encoding", help="コピー句ファイルの文字コード (既定: 自動判定)")
    args = ap.parse_args(argv)

    if not os.path.isfile(args.input):
        ap.error(f"入力ファイルが見つかりません: {args.input}")

    n_src = sum([bool(args.copybook), bool(args.layout), bool(args.field), args.guess])
    if n_src == 0:
        ap.error("レイアウトを指定してください: -c コピー句 / -l レイアウトCSV / --field / --guess")
    if n_src > 1:
        ap.error("レイアウト指定 (-c / -l / --field / --guess) は1つだけにしてください")

    # --- レイアウト解決 ---
    record_length = args.record_length
    if args.copybook:
        nodes = parse_copybook(args.copybook, args.copybook_encoding)
        fields, total = copybook_to_fields(nodes)
        if not fields:
            ap.error("コピー句からフィールドを取得できませんでした")
        if record_length is None:
            record_length = total
        elif record_length < total:
            ap.error(f"--record-length ({record_length}) がコピー句の合計 ({total}) より小さいです")
    elif args.layout:
        fields = load_layout_csv(args.layout)
        if record_length is None:
            record_length = max(f.start + f.length for f in fields)
    elif args.field:
        fields = parse_field_args(args.field)
        if record_length is None:
            record_length = max(f.start + f.length for f in fields)
    else:  # guess
        if record_length is None:
            print("レコード長を自動検出しています...", file=sys.stderr)
            record_length, cands = detect_record_length(args.input)
            if record_length is None:
                ap.error("レコード長を検出できませんでした。--record-length で指定してください")
            cand_str = ", ".join(f"{rl}({s:.0%})" for rl, s in cands)
            print(f"レコード長: {record_length} バイトと推定 (候補: {cand_str})", file=sys.stderr)
        with open(args.input, "rb") as fp:
            sample = fp.read(record_length * 1000)
        nsamp = len(sample) // record_length
        if nsamp == 0:
            ap.error(f"ファイルが1レコード ({record_length} バイト) 未満です")
        records = [sample[i * record_length:(i + 1) * record_length] for i in range(nsamp)]
        fields = guess_layout(records)
        layout_path = args.save_layout or (args.input + ".layout.csv")
        save_layout_csv(fields, layout_path)
        print(f"推測レイアウトを保存しました: {layout_path}", file=sys.stderr)
        print("※ 推測結果です。保存されたCSVを確認・修正のうえ -l で再実行することを推奨します。",
              file=sys.stderr)

    # 出力形式の決定
    if args.format is None:
        if args.output and args.output.lower().endswith(".csv"):
            args.format = "csv"
        else:
            args.format = "text"

    print(format_layout(fields, record_length), file=sys.stderr)
    if args.save_layout and not args.guess:
        save_layout_csv(fields, args.save_layout)
        print(f"レイアウトを保存しました: {args.save_layout}", file=sys.stderr)
    if args.show_layout:
        return

    # レコード長の妥当性チェック (レイアウトから導出した場合はズレを許さない)
    file_size = os.path.getsize(args.input)
    if file_size % record_length != 0 and args.record_length is None and \
            (args.layout or args.field):
        ap.error(
            f"ファイルサイズ {file_size:,} がレイアウトから導出したレコード長 "
            f"{record_length} の倍数ではありません。フィールドがレコードの一部しか"
            f"カバーしていない可能性があります。-r/--record-length で実際の"
            f"レコード長を指定してください")

    # 範囲チェック
    over = [f for f in fields if f.start + f.length > record_length]
    if over:
        ap.error(f"レコード長 {record_length} を超えるフィールドがあります: "
                 + ", ".join(f"{f.name}(終端{f.start + f.length})" for f in over[:5]))

    process(args, fields, record_length)


if __name__ == "__main__":
    main()
