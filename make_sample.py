#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""テスト用サンプルデータ生成 (SJIS・改行なし・パック項目あり)"""
import random
import sys

COPYBOOK = """\
       01  SHAIN-REC.
           05  SHAIN-NO      PIC 9(5).
           05  NAME-KANA     PIC X(10).
           05  NAME-KANJI    PIC N(5).
           05  KYUYO         PIC S9(7)V99 COMP-3.
           05  BUSHO-CD      PIC S9(4)    COMP.
           05  NYUSHA-YMD    PIC 9(8).
           05  BIKO          PIC X(6).
"""
# レコード長 = 5 + 10 + 10 + 5 + 2 + 8 + 6 = 46 バイト

KANA = ["ﾀﾅｶ ﾀﾛｳ", "ｽｽﾞｷ ﾊﾅｺ", "ｻﾄｳ ｼﾞﾛｳ", "ﾔﾏﾀﾞ ｹﾝ", "ｲﾄｳ ﾕｷ"]
KANJI = ["田中太郎", "鈴木花子", "佐藤次郎", "山田健", "伊藤幸"]


def pack(value: int, digits: int, scale: int = 0) -> bytes:
    """符号付きパック10進 (COMP-3) にエンコード"""
    neg = value < 0
    s = str(abs(value)).rjust(digits, "0")
    if len(s) % 2 == 0:  # ニブル数 = 桁数+1(符号) が偶数になるよう調整
        s = "0" + s
    return bytes.fromhex(s + ("d" if neg else "c"))


def make_record(i: int) -> bytes:
    k = random.randrange(len(KANA))
    rec = b""
    rec += f"{i % 100000:05d}".encode("cp932")                    # SHAIN-NO 9(5)
    rec += KANA[k].encode("cp932").ljust(10)[:10]                 # NAME-KANA X(10)
    rec += KANJI[k].ljust(5, "　").encode("cp932")[:10]       # NAME-KANJI N(5)
    kyuyo = random.randint(-99999, 9999999)                       # S9(7)V99 COMP-3
    rec += pack(kyuyo, 9)                                         # 9桁 -> 5バイト
    rec += random.randint(1, 9999).to_bytes(2, "big")             # BUSHO-CD COMP 2バイト
    rec += f"{random.randint(1990, 2026)}{random.randint(1, 12):02d}{random.randint(1, 28):02d}".encode()
    rec += "ﾋﾞｺｳ".encode("cp932").ljust(6)[:6]                    # BIKO X(6)
    assert len(rec) == 46, len(rec)
    return rec


def main():
    n = int(sys.argv[1]) if len(sys.argv) > 1 else 100
    out = sys.argv[2] if len(sys.argv) > 2 else "sample.dat"
    random.seed(42)
    with open(out, "wb") as f:
        for i in range(1, n + 1):
            f.write(make_record(i))
    with open("sample.cpy", "w", encoding="cp932") as f:
        f.write(COPYBOOK)
    print(f"{out} ({n} レコード, 46 バイト/レコード) と sample.cpy を生成しました")


if __name__ == "__main__":
    main()
