#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
hostparse_gui.py — ホスト固定長ファイル パースツール (GUI版)

SJIS・改行なしの固定長レコードファイルを、コピー句 / 手動レイアウト / 推測 に
もとづいてパースし、テキストまたはCSVで出力する。エンジンは hostparse.py を利用。

起動:  python hostparse_gui.py  (または hostparse_gui.bat をダブルクリック)
"""

import csv
import os
import queue
import sys
import threading
import time
import tkinter as tk
from tkinter import filedialog, messagebox, ttk

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import hostparse as hp

# 形式コード <-> 表示ラベル
TYPE_LABELS = [
    ("X", "X: 半角文字"),
    ("K", "K: 半角カナ"),
    ("N", "N: 全角文字"),
    ("9", "9: ゾーン数値"),
    ("S9", "S9: 符号付数値"),
    ("P", "P: パック(COMP-3)"),
    ("B", "B: バイナリ(COMP)"),
]
CODE2LABEL = dict(TYPE_LABELS)
LABEL2CODE = {v: k for k, v in TYPE_LABELS}

PREVIEW_RECORDS = 200
DUMP_RECORDS = 5        # 16進ダンプに表示するレコード数
DUMP_BYTES = 512        # レコード長が未確定のときにダンプするバイト数


def field_type_code(f: hp.Field) -> str:
    if f.ftype == "9" and f.signed:
        return "S9"
    return f.ftype


class App(tk.Tk):
    def __init__(self):
        super().__init__()
        self.title("hostparse — ホスト固定長ファイル パースツール")
        # 高DPI対応: OSのスケール率に合わせて tk とウィンドウサイズを拡大
        try:
            import ctypes
            dpi = ctypes.windll.user32.GetDpiForSystem()
        except Exception:
            dpi = 96
        scale = dpi / 96
        self.tk.call("tk", "scaling", dpi / 72)
        self.geometry(f"{int(1180 * scale)}x{int(800 * scale)}")
        self.minsize(int(900 * scale), int(620 * scale))

        self.fields = []          # list[hp.Field]
        self.msg_q = queue.Queue()
        self.converting = False
        self._input_size = None   # 入力ファイルサイズ (パス変更時のみ取得)
        self._bg_running = set()  # 実行中のバックグラウンド処理キー
        self._prev_cols_sig = None  # プレビューの列構成 (変化時のみ組み直す)
        self._preview_status = None  # プレビュー完了時に出すステータス (上書き用)
        self._guess_after_detect = False  # 自動検出の完了後に推測へ続けるか
        self._input_reload_job = None     # 入力パス変更のデバウンス用

        self._build_ui()
        # キー -> 完了時にUIスレッドで呼ぶハンドラ (result, err, elapsed)
        self._bg_handlers = {
            "fileinfo": self._on_fileinfo,
            "dump": self._on_dump,
            "detect": self._on_detect,
            "guess": self._on_guess,
            "preview": self._on_preview,
        }
        self.after(100, self._poll_queue)

    # ------------------------------------------------- バックグラウンド実行
    def _run_bg(self, key, fn, busy_msg):
        """fn() をワーカースレッドで実行する。

        fn はファイルI/Oと純粋計算のみを行い、Tk ウィジェットには一切触れない。
        結果は ("bg", key, result, err) としてキューに載り、UIスレッド上の
        self._bg_handlers[key] が受け取る。
        """
        if key in self._bg_running:
            return False        # 同じ処理の多重実行は無視する
        self._bg_running.add(key)
        self._update_busy()
        self.var_status.set(busy_msg)

        def worker():
            t0 = time.perf_counter()
            try:
                result = fn()
            except Exception as e:       # noqa: BLE001 - UI側でまとめて通知する
                self.msg_q.put(("bg", key, None, e, 0.0))
            else:
                self.msg_q.put(("bg", key, result, None,
                                time.perf_counter() - t0))

        threading.Thread(target=worker, daemon=True).start()
        return True

    def _update_busy(self):
        """処理中はカーソルを待機表示にし、ファイルを触るボタンを止める。"""
        busy = bool(self._bg_running)
        self.configure(cursor="watch" if busy else "")
        state = "disabled" if busy else "normal"
        for btn in self._busy_buttons:
            btn.configure(state=state)

    # ------------------------------------------------------------------ UI
    def _build_ui(self):
        pad = {"padx": 4, "pady": 2}
        self._busy_buttons = []   # 処理中に無効化するボタン

        # ===== 入力ファイル =====
        frm_file = ttk.LabelFrame(self, text=" 入力ファイル (SJIS・改行なし固定長) ")
        frm_file.pack(fill="x", padx=8, pady=(8, 4))
        self.var_input = tk.StringVar()
        ttk.Entry(frm_file, textvariable=self.var_input).grid(
            row=0, column=0, sticky="ew", **pad)
        btn = ttk.Button(frm_file, text="参照...", command=self.pick_input)
        btn.grid(row=0, column=1, **pad)
        self._busy_buttons.append(btn)
        ttk.Label(frm_file, text="レコード長:").grid(row=0, column=2, **pad)
        self.var_reclen = tk.StringVar()
        ent = ttk.Entry(frm_file, textvariable=self.var_reclen, width=8)
        ent.grid(row=0, column=3, **pad)
        btn = ttk.Button(frm_file, text="自動検出", command=self.detect_reclen)
        btn.grid(row=0, column=4, **pad)
        self._busy_buttons.append(btn)
        self.var_fileinfo = tk.StringVar(value="ファイル未選択")
        ttk.Label(frm_file, textvariable=self.var_fileinfo, foreground="#555").grid(
            row=1, column=0, columnspan=5, sticky="w", **pad)
        frm_file.columnconfigure(0, weight=1)
        self.var_reclen.trace_add("write", lambda *a: self._update_fileinfo())
        self.var_input.trace_add("write", lambda *a: self._schedule_input_reload())

        # ===== 中央: レイアウト(左) + プレビュー(右) =====
        # (pack は _build_ui 末尾で行う: 下部セクションの表示領域を先に確保するため)
        paned = ttk.PanedWindow(self, orient="horizontal")

        # ---- レイアウト ----
        frm_layout = ttk.LabelFrame(paned, text=" レイアウト ")
        paned.add(frm_layout, weight=2)

        bar = ttk.Frame(frm_layout)
        bar.pack(fill="x", padx=4, pady=2)
        for text, cmd in (("コピー句 読込", self.load_copybook),
                          ("レイアウトCSV 読込", self.load_layout),
                          ("推測", self.guess)):
            btn = ttk.Button(bar, text=text, command=cmd)
            btn.pack(side="left", padx=2)
            self._busy_buttons.append(btn)
        ttk.Button(bar, text="CSV保存", command=self.save_layout).pack(
            side="left", padx=2)
        ttk.Button(bar, text="全クリア", command=self.clear_layout).pack(
            side="right", padx=2)

        cols = ("name", "start", "length", "type", "scale", "end")
        heads = ("名前", "開始", "バイト", "形式", "小数", "終端")
        widths = (140, 45, 45, 110, 40, 45)
        self.tree = ttk.Treeview(frm_layout, columns=cols, show="headings",
                                 selectmode="browse")
        for c, h, w in zip(cols, heads, widths):
            self.tree.heading(c, text=h)
            anchor = "w" if c in ("name", "type") else "e"
            self.tree.column(c, width=w, anchor=anchor, stretch=(c == "name"))
        ysb = ttk.Scrollbar(frm_layout, orient="vertical", command=self.tree.yview)
        self.tree.configure(yscrollcommand=ysb.set)
        self.tree.pack(side="left", fill="both", expand=True, padx=(4, 0), pady=2)
        ysb.pack(side="left", fill="y", pady=2)
        self.tree.bind("<<TreeviewSelect>>", self.on_tree_select)

        # ---- フィールド編集欄 ----
        frm_edit = ttk.LabelFrame(self, text=" フィールド編集 (行を選択→修正→更新 / 新規は追加) ")
        self.var_fname = tk.StringVar()
        self.var_fstart = tk.StringVar()
        self.var_flen = tk.StringVar()
        self.var_ftype = tk.StringVar(value=TYPE_LABELS[0][1])
        self.var_fscale = tk.StringVar()
        ttk.Label(frm_edit, text="名前").grid(row=0, column=0, **pad)
        ttk.Entry(frm_edit, textvariable=self.var_fname, width=18).grid(
            row=0, column=1, **pad)
        ttk.Label(frm_edit, text="開始(1始まり)").grid(row=0, column=2, **pad)
        ttk.Entry(frm_edit, textvariable=self.var_fstart, width=7).grid(
            row=0, column=3, **pad)
        ttk.Label(frm_edit, text="バイト数").grid(row=0, column=4, **pad)
        ttk.Entry(frm_edit, textvariable=self.var_flen, width=7).grid(
            row=0, column=5, **pad)
        ttk.Label(frm_edit, text="形式").grid(row=0, column=6, **pad)
        ttk.Combobox(frm_edit, textvariable=self.var_ftype, width=18,
                     values=[l for _, l in TYPE_LABELS], state="readonly").grid(
            row=0, column=7, **pad)
        ttk.Label(frm_edit, text="小数桁").grid(row=0, column=8, **pad)
        ttk.Entry(frm_edit, textvariable=self.var_fscale, width=5).grid(
            row=0, column=9, **pad)
        ttk.Button(frm_edit, text="追加", command=self.add_field).grid(
            row=0, column=10, **pad)
        ttk.Button(frm_edit, text="更新", command=self.update_field).grid(
            row=0, column=11, **pad)
        ttk.Button(frm_edit, text="削除", command=self.delete_field).grid(
            row=0, column=12, **pad)

        # ---- プレビュー / ダンプ ----
        nb = ttk.Notebook(paned)
        paned.add(nb, weight=3)

        frm_prev = ttk.Frame(nb)
        nb.add(frm_prev, text=f" プレビュー (先頭{PREVIEW_RECORDS}件) ")
        bar2 = ttk.Frame(frm_prev)
        bar2.pack(fill="x")
        btn = ttk.Button(bar2, text="プレビュー更新", command=self.refresh_preview)
        btn.pack(side="left", padx=2, pady=2)
        self._busy_buttons.append(btn)
        self.var_previnfo = tk.StringVar()
        ttk.Label(bar2, textvariable=self.var_previnfo, foreground="#555").pack(
            side="left", padx=8)
        self.prev = ttk.Treeview(frm_prev, show="headings")
        pysb = ttk.Scrollbar(frm_prev, orient="vertical", command=self.prev.yview)
        pxsb = ttk.Scrollbar(frm_prev, orient="horizontal", command=self.prev.xview)
        self.prev.configure(yscrollcommand=pysb.set, xscrollcommand=pxsb.set)
        pxsb.pack(side="bottom", fill="x")
        self.prev.pack(side="left", fill="both", expand=True)
        pysb.pack(side="left", fill="y")

        frm_dump = ttk.Frame(nb)
        nb.add(frm_dump, text=" 16進ダンプ ")
        self.dump = tk.Text(frm_dump, font=("MS Gothic", 10), wrap="none",
                            state="disabled")
        dysb = ttk.Scrollbar(frm_dump, orient="vertical", command=self.dump.yview)
        dxsb = ttk.Scrollbar(frm_dump, orient="horizontal", command=self.dump.xview)
        self.dump.configure(yscrollcommand=dysb.set, xscrollcommand=dxsb.set)
        dxsb.pack(side="bottom", fill="x")
        self.dump.pack(side="left", fill="both", expand=True)
        dysb.pack(side="left", fill="y")

        # ===== 出力 =====
        frm_out = ttk.LabelFrame(self, text=" 出力 ")
        self.var_format = tk.StringVar(value="csv")
        self.var_encoding = tk.StringVar(value="utf-8")
        self.var_keepzeros = tk.BooleanVar(value=False)
        ttk.Radiobutton(frm_out, text="CSV", variable=self.var_format,
                        value="csv").grid(row=0, column=0, **pad)
        ttk.Radiobutton(frm_out, text="テキスト(桁揃え)", variable=self.var_format,
                        value="text").grid(row=0, column=1, **pad)
        ttk.Separator(frm_out, orient="vertical").grid(
            row=0, column=2, sticky="ns", padx=6)
        ttk.Radiobutton(frm_out, text="UTF-8", variable=self.var_encoding,
                        value="utf-8").grid(row=0, column=3, **pad)
        ttk.Radiobutton(frm_out, text="ANSI (SJIS)", variable=self.var_encoding,
                        value="ansi").grid(row=0, column=4, **pad)
        ttk.Checkbutton(frm_out, text="先頭ゼロを残す",
                        variable=self.var_keepzeros).grid(row=0, column=5, **pad)
        self.var_output = tk.StringVar()
        ttk.Entry(frm_out, textvariable=self.var_output).grid(
            row=1, column=0, columnspan=5, sticky="ew", **pad)
        ttk.Button(frm_out, text="参照...", command=self.pick_output).grid(
            row=1, column=5, **pad)
        self.btn_convert = ttk.Button(frm_out, text="変換実行",
                                      command=self.convert)
        self.btn_convert.grid(row=1, column=6, **pad)
        self.progress = ttk.Progressbar(frm_out, maximum=1000)
        self.progress.grid(row=2, column=0, columnspan=7, sticky="ew", **pad)
        frm_out.columnconfigure(0, weight=1)

        # ===== ステータスバー =====
        self.var_status = tk.StringVar(value="入力ファイルを選択してください")
        status = ttk.Label(self, textvariable=self.var_status, relief="sunken",
                           anchor="w")

        # ===== pack (下から順に領域を確保し、残りを中央ペインへ) =====
        status.pack(fill="x", side="bottom")
        frm_out.pack(fill="x", side="bottom", padx=8, pady=4)
        frm_edit.pack(fill="x", side="bottom", padx=8, pady=4)
        paned.pack(fill="both", expand=True, padx=8, pady=4)

    # ------------------------------------------------------------ 入力ファイル
    def pick_input(self):
        path = filedialog.askopenfilename(
            title="入力ファイルを選択",
            filetypes=[("すべてのファイル", "*.*"), ("データ", "*.dat *.txt *.bin")])
        if not path:
            return
        self.var_input.set(path)
        base, _ = os.path.splitext(path)
        self.var_output.set(base + "_変換.csv")
        self._schedule_input_reload(delay=0)   # 明示的な選択なので待たない

    def _schedule_input_reload(self, delay=300):
        """入力パスの変更をまとめて処理する (直接入力中にファイルを叩かない)。"""
        if self._input_reload_job is not None:
            self.after_cancel(self._input_reload_job)
        self._input_size = None
        self._input_reload_job = self.after(delay, self._reload_input)

    def _reload_input(self):
        self._input_reload_job = None
        path = self.var_input.get().strip()
        if not path:
            self.var_fileinfo.set("ファイル未選択")
            return
        self.var_fileinfo.set("ファイル情報を読込んでいます...")
        # ネットワークドライブやクラウド同期フォルダでは getsize/read 自体が
        # ブロックしうるため、サイズ取得もダンプもワーカースレッドで行う。
        if not self._run_bg("fileinfo", lambda: os.path.getsize(path),
                            "ファイル情報を読込んでいます..."):
            self._input_reload_job = self.after(200, self._reload_input)

    def _on_fileinfo(self, size, err, elapsed=0.0):
        if err:
            self._input_size = None
            self.var_fileinfo.set("ファイルを読めません")
            self.var_status.set(f"入力ファイルを読めません: {err}")
            return
        self._input_size = size
        self._update_fileinfo()
        self.refresh_dump()
        self.var_status.set("レイアウトを指定してください (コピー句読込 / 推測 / 手動追加)")

    def _update_fileinfo(self):
        """キャッシュ済みサイズから表示を組み立てる (ファイルには触らない)。"""
        if self._input_size is None:
            self.var_fileinfo.set("ファイル情報を読込んでいます..."
                                  if self.var_input.get().strip()
                                  else "ファイル未選択")
            return
        size = self._input_size
        rl = self._get_reclen(silent=True)
        if rl:
            nrec, rem = divmod(size, rl)
            warn = f" ※末尾{rem}バイト余り" if rem else ""
            self.var_fileinfo.set(
                f"サイズ: {size:,} バイト / {nrec:,} レコード{warn}")
        else:
            self.var_fileinfo.set(f"サイズ: {size:,} バイト")

    def _get_reclen(self, silent=False):
        s = self.var_reclen.get().strip()
        if not s:
            if not silent:
                messagebox.showwarning("レコード長", "レコード長を入力するか自動検出してください")
            return None
        try:
            v = int(s)
            if v <= 0:
                raise ValueError
            return v
        except ValueError:
            if not silent:
                messagebox.showerror("レコード長", f"レコード長が不正です: {s}")
            return None

    def _need_input(self):
        path = self.var_input.get()
        if not os.path.isfile(path):
            messagebox.showwarning("入力ファイル", "入力ファイルを選択してください")
            return None
        return path

    def detect_reclen(self):
        path = self._need_input()
        if not path:
            return
        self._run_bg("detect", lambda: hp.detect_record_length(path),
                     "レコード長を検出しています...")

    def _on_detect(self, result, err, elapsed=0.0):
        chain_guess, self._guess_after_detect = self._guess_after_detect, False
        if err:
            self.var_status.set(f"レコード長の検出に失敗しました: {err}")
            messagebox.showerror("自動検出", f"レコード長を検出できませんでした:\n{err}")
            return
        rl, cands = result
        if rl is None:
            messagebox.showwarning(
                "自動検出", "レコード長を検出できませんでした。\n"
                "ファイルサイズがレコード長の倍数でない可能性があります。手入力してください。")
            self.var_status.set("レコード長の検出に失敗しました")
            return
        self.var_reclen.set(str(rl))
        self._update_fileinfo()
        self.refresh_dump()
        if chain_guess:
            self.guess()
            return
        cand_str = ", ".join(f"{r}({s:.0%})" for r, s in cands)
        self.var_status.set(       # 後続処理のメッセージを消さないよう最後に表示
            f"レコード長 {rl} バイトと推定 (候補: {cand_str}) / {elapsed:.2f}秒")

    # ------------------------------------------------------------ レイアウト
    def _refresh_tree(self):
        self.tree.delete(*self.tree.get_children())
        for i, f in enumerate(self.fields):
            self.tree.insert("", "end", iid=str(i), values=(
                f.name, f.start + 1, f.length,
                CODE2LABEL.get(field_type_code(f), f.ftype),
                f.scale or "", f.start + f.length))

    def on_tree_select(self, _ev=None):
        sel = self.tree.selection()
        if not sel:
            return
        f = self.fields[int(sel[0])]
        self.var_fname.set(f.name)
        self.var_fstart.set(str(f.start + 1))
        self.var_flen.set(str(f.length))
        self.var_ftype.set(CODE2LABEL.get(field_type_code(f), TYPE_LABELS[0][1]))
        self.var_fscale.set(str(f.scale) if f.scale else "")

    def _read_edit_fields(self):
        try:
            start = int(self.var_fstart.get())
            length = int(self.var_flen.get())
            if start < 1 or length < 1:
                raise ValueError
        except ValueError:
            messagebox.showerror("フィールド編集", "開始位置(1始まり)とバイト数は正の整数で入力してください")
            return None
        code = LABEL2CODE.get(self.var_ftype.get(), "X")
        ftype, signed = hp.normalize_type(code)
        scale = 0
        s = self.var_fscale.get().strip()
        if s:
            try:
                scale = int(s)
            except ValueError:
                messagebox.showerror("フィールド編集", f"小数桁が不正です: {s}")
                return None
        name = self.var_fname.get().strip() or f"F{len(self.fields) + 1:03d}"
        return hp.Field(name=name, start=start - 1, length=length,
                        ftype=ftype, scale=scale, signed=signed)

    def add_field(self):
        # 開始位置が空なら直前フィールドの終端+1を補完
        if not self.var_fstart.get().strip() and self.fields:
            last = max(f.start + f.length for f in self.fields)
            self.var_fstart.set(str(last + 1))
        elif not self.var_fstart.get().strip():
            self.var_fstart.set("1")
        f = self._read_edit_fields()
        if not f:
            return
        self.fields.append(f)
        self._refresh_tree()
        # 次の追加に備えて開始位置を進める
        self.var_fname.set("")
        self.var_fstart.set(str(f.start + f.length + 1))
        self.var_status.set(f"フィールド {f.name} を追加しました (計{len(self.fields)}件)")

    def update_field(self):
        sel = self.tree.selection()
        if not sel:
            messagebox.showinfo("更新", "更新する行を一覧から選択してください")
            return
        f = self._read_edit_fields()
        if not f:
            return
        self.fields[int(sel[0])] = f
        self._refresh_tree()
        self.tree.selection_set(sel[0])
        self.var_status.set(f"フィールド {f.name} を更新しました")

    def delete_field(self):
        sel = self.tree.selection()
        if not sel:
            messagebox.showinfo("削除", "削除する行を一覧から選択してください")
            return
        f = self.fields.pop(int(sel[0]))
        self._refresh_tree()
        self.var_status.set(f"フィールド {f.name} を削除しました")

    def clear_layout(self):
        if self.fields and not messagebox.askyesno("全クリア", "レイアウトをすべて削除しますか?"):
            return
        self.fields = []
        self._refresh_tree()

    def load_copybook(self):
        path = filedialog.askopenfilename(
            title="コピー句ファイルを選択",
            filetypes=[("コピー句", "*.cpy *.cbl *.cob *.txt"), ("すべて", "*.*")])
        if not path:
            return
        try:
            nodes = hp.parse_copybook(path)
            fields, total = hp.copybook_to_fields(nodes)
            if not fields:
                raise ValueError("データ項目が見つかりませんでした")
        except Exception as e:
            messagebox.showerror("コピー句読込", f"コピー句を解析できませんでした:\n{e}")
            return
        self.fields = fields
        self.var_reclen.set(str(total))
        self._refresh_tree()
        self._update_fileinfo()
        self.var_status.set(
            f"コピー句を読込みました: {len(fields)}項目 / レコード長 {total} バイト")
        if self.var_input.get():
            self.refresh_preview()

    def load_layout(self):
        path = filedialog.askopenfilename(
            title="レイアウトCSVを選択",
            filetypes=[("CSV", "*.csv"), ("すべて", "*.*")])
        if not path:
            return
        try:
            fields = hp.load_layout_csv(path)
        except Exception as e:
            messagebox.showerror("レイアウト読込", f"レイアウトCSVを読めませんでした:\n{e}")
            return
        self.fields = fields
        if not self.var_reclen.get().strip():
            self.var_reclen.set(str(max(f.start + f.length for f in fields)))
        self._refresh_tree()
        self._update_fileinfo()
        self.var_status.set(f"レイアウトCSVを読込みました: {len(fields)}項目")
        if self.var_input.get():
            self.refresh_preview()

    def save_layout(self):
        if not self.fields:
            messagebox.showinfo("CSV保存", "保存するレイアウトがありません")
            return
        path = filedialog.asksaveasfilename(
            title="レイアウトCSVを保存", defaultextension=".csv",
            filetypes=[("CSV", "*.csv")])
        if not path:
            return
        hp.save_layout_csv(self.fields, path)
        self.var_status.set(f"レイアウトを保存しました: {path}")

    def guess(self):
        path = self._need_input()
        if not path:
            return
        rl = self._get_reclen(silent=True)
        if not rl:
            # レコード長が未設定なら先に検出し、その完了後に改めて推測する
            if self._run_bg("detect", lambda: hp.detect_record_length(path),
                            "レコード長を検出しています..."):
                self._guess_after_detect = True
            return
        self._run_bg("guess", lambda: self._guess_worker(path, rl),
                     "レイアウトを推測しています...")

    @staticmethod
    def _guess_worker(path, rl):
        with open(path, "rb") as fp:
            sample = fp.read(rl * 1000)
        nsamp = len(sample) // rl
        if nsamp == 0:
            return None
        records = [sample[i * rl:(i + 1) * rl] for i in range(nsamp)]
        return hp.guess_layout(records)

    def _on_guess(self, fields, err, elapsed=0.0):
        if err:
            self.var_status.set(f"推測に失敗しました: {err}")
            messagebox.showerror("推測", f"レイアウトを推測できませんでした:\n{err}")
            return
        if fields is None:
            rl = self._get_reclen(silent=True)
            messagebox.showwarning("推測", f"ファイルが1レコード({rl}バイト)未満です")
            return
        self.fields = fields
        self._refresh_tree()
        # プレビュー完了時のメッセージで推測の注意書きが消えないよう明示的に渡す
        self.refresh_preview(status=(
            f"レイアウトを推測しました: {len(self.fields)}項目 ({elapsed:.2f}秒) "
            "※推測結果です。プレビューを確認し、必要に応じて修正してください "
            "(パックの小数桁・バイナリ項目は判別できません)"))

    # ------------------------------------------------------------ プレビュー
    def refresh_preview(self, status=None):
        """status を渡すと、完了時のステータス表示をそれで上書きする。"""
        path = self._need_input()
        if not path:
            return
        if not self.fields:
            messagebox.showinfo("プレビュー", "レイアウトがありません")
            return
        rl = self._get_reclen()
        if not rl:
            return
        over = [f for f in self.fields if f.start + f.length > rl]
        if over:
            messagebox.showerror(
                "プレビュー",
                f"レコード長 {rl} を超えるフィールドがあります: "
                + ", ".join(f"{f.name}(終端{f.start + f.length})" for f in over[:5]))
            return

        fields = list(self.fields)
        keep_zeros = self.var_keepzeros.get()
        self._preview_status = status
        self._run_bg("preview",
                     lambda: self._preview_worker(path, rl, fields, keep_zeros),
                     "プレビューを作成しています...")

    @staticmethod
    def _preview_worker(path, rl, fields, keep_zeros):
        """先頭 PREVIEW_RECORDS 件だけを読んで行データを組み立てる。

        読込量はレコード長 × 件数で頭打ちなので、ファイルサイズには依存しない。
        """
        errors = hp.ErrorCounter()
        convs = [(slice(f.start, f.start + f.length),
                  hp.make_converter(f, True, keep_zeros, errors))
                 for f in fields]
        with open(path, "rb") as fp:
            data = fp.read(rl * PREVIEW_RECORDS)
        rows = [[fn(data[off + sl.start:off + sl.stop]) for sl, fn in convs]
                for off in range(0, (len(data) // rl) * rl, rl)]
        return fields, rows, errors.count

    def _on_preview(self, result, err, elapsed=0.0):
        if err:
            self.var_status.set(f"プレビューに失敗しました: {err}")
            messagebox.showerror("プレビュー", f"プレビューを作成できませんでした:\n{err}")
            return
        fields, rows, nerr = result
        # 列構成が変わっていなければ組み直さず、行の入替えだけで済ませる
        sig = tuple((f.name, f.ftype, f.length, f.scale) for f in fields)
        if sig != self._prev_cols_sig:
            self.prev.configure(columns=list(range(len(fields))))
            for i, f in enumerate(fields):
                self.prev.heading(i, text=f.name)
                w = max(60, min(240, max(len(f.name), hp.display_width(f)) * 9))
                self.prev.column(i, width=w,
                                 anchor="e" if f.ftype in "9PB" else "w",
                                 stretch=False)
            self._prev_cols_sig = sig
        self.prev.delete(*self.prev.get_children())
        for row in rows:
            self.prev.insert("", "end", values=row)
        info = f"{len(rows)} 件表示 / {elapsed:.2f}秒"
        if nerr:
            info += f" / 変換エラー {nerr} 件 (#ERR表示) — レイアウトを確認してください"
        self.var_previnfo.set(info)
        self.var_status.set(self._preview_status
                            or f"プレビューを更新しました ({info})")

    def refresh_dump(self):
        path = self.var_input.get()
        if not path:
            return
        rl = self._get_reclen(silent=True)
        self._run_bg("dump", lambda: self._dump_worker(path, rl),
                     "16進ダンプを作成しています...")

    @staticmethod
    def _dump_worker(path, rl):
        """先頭 DUMP_RECORDS 件 (レコード長未確定なら DUMP_BYTES) だけダンプする。"""
        with open(path, "rb") as fp:
            data = fp.read(rl * DUMP_RECORDS if rl else DUMP_BYTES)

        def hex_lines(seq):
            out = []
            for off in range(0, len(seq), 16):
                seg = seq[off:off + 16]
                hexs = " ".join(f"{b:02X}" for b in seg).ljust(47)
                chars = seg.decode("cp932", "replace").replace("�", "･")
                out.append(f"{off + 1:>5}: {hexs} | {chars}")
            return out

        lines = []
        if rl:
            # レコード単位: 1行16バイトで位置(1始まり)を表示
            for r in range(min(DUMP_RECORDS, len(data) // rl)):
                lines.append(f"===== レコード {r + 1} =====")
                lines += hex_lines(data[r * rl:(r + 1) * rl])
                lines.append("")
        else:
            lines = hex_lines(data)
        return "\n".join(lines)

    def _on_dump(self, text, err, elapsed=0.0):
        if err:
            self.var_status.set(f"16進ダンプを作成できませんでした: {err}")
            return
        self.dump.configure(state="normal")
        self.dump.delete("1.0", "end")
        self.dump.insert("1.0", text)
        self.dump.configure(state="disabled")

    # ------------------------------------------------------------ 変換
    def pick_output(self):
        ext = ".csv" if self.var_format.get() == "csv" else ".txt"
        path = filedialog.asksaveasfilename(
            title="出力ファイル", defaultextension=ext,
            filetypes=[("CSV", "*.csv"), ("テキスト", "*.txt"), ("すべて", "*.*")])
        if path:
            self.var_output.set(path)

    def convert(self):
        if self.converting:
            return
        path = self._need_input()
        if not path:
            return
        if not self.fields:
            messagebox.showwarning("変換", "レイアウトを指定してください")
            return
        rl = self._get_reclen()
        if not rl:
            return
        over = [f for f in self.fields if f.start + f.length > rl]
        if over:
            messagebox.showerror(
                "変換", f"レコード長 {rl} を超えるフィールドがあります: "
                + ", ".join(f"{f.name}(終端{f.start + f.length})" for f in over[:5]))
            return
        out = self.var_output.get().strip()
        if not out:
            messagebox.showwarning("変換", "出力ファイルを指定してください")
            return
        size = os.path.getsize(path)
        if size % rl != 0:
            if not messagebox.askyesno(
                    "確認", f"ファイルサイズ {size:,} がレコード長 {rl} の倍数では"
                    f"ありません (末尾 {size % rl} バイトは無視されます)。\n続行しますか?"):
                return
        if os.path.abspath(out) == os.path.abspath(path):
            messagebox.showerror("変換", "出力先が入力ファイルと同じです")
            return

        self.converting = True
        self.btn_convert.configure(state="disabled")
        self.progress["value"] = 0
        self.var_status.set("変換中...")
        args = (path, out, list(self.fields), rl, self.var_format.get(),
                self.var_encoding.get(), self.var_keepzeros.get())
        threading.Thread(target=self._convert_worker, args=args,
                         daemon=True).start()

    def _convert_worker(self, path, out, fields, rl, fmt, enc, keep_zeros):
        try:
            errors = hp.ErrorCounter()
            is_csv = fmt == "csv"
            convs = [(slice(f.start, f.start + f.length),
                      hp.make_converter(f, is_csv, keep_zeros, errors))
                     for f in fields]
            widths = [hp.display_width(f) for f in fields]
            numeric = [f.ftype in ("9", "P", "B") for f in fields]
            out_enc = "cp932" if enc == "ansi" else "utf-8"
            if is_csv and out_enc == "utf-8":
                out_enc = "utf-8-sig"
            total = os.path.getsize(path)
            done = 0
            nrec = 0
            chunk_size = max(1, (8 * 1024 * 1024) // rl) * rl
            mode = dict(newline="") if is_csv else {}
            with open(out, "w", encoding=out_enc, errors="replace", **mode) as ofp, \
                    open(path, "rb") as ifp:
                writer = csv.writer(ofp) if is_csv else None
                if is_csv:
                    writer.writerow([f.name for f in fields])
                while True:
                    chunk = ifp.read(chunk_size)
                    if len(chunk) < rl:
                        break
                    usable = (len(chunk) // rl) * rl
                    if is_csv:
                        rows = [[fn(chunk[o + sl.start:o + sl.stop])
                                 for sl, fn in convs]
                                for o in range(0, usable, rl)]
                        writer.writerows(rows)
                        nrec += len(rows)
                    else:
                        buf = []
                        for o in range(0, usable, rl):
                            row = [fn(chunk[o + sl.start:o + sl.stop])
                                   for sl, fn in convs]
                            buf.append(" ".join(
                                (v.rjust(w) if num else v.ljust(w)
                                 if len(v) <= w else v)
                                for v, w, num in zip(row, widths, numeric)))
                            nrec += 1
                        ofp.write("\n".join(buf) + "\n")
                    done += len(chunk)
                    self.msg_q.put(("progress", done / total))
            self.msg_q.put(("done", nrec, errors.count, out))
        except Exception as e:
            self.msg_q.put(("error", str(e)))

    def _poll_queue(self):
        try:
            while True:
                msg = self.msg_q.get_nowait()
                if msg[0] == "bg":
                    _, key, result, err, elapsed = msg
                    self._bg_running.discard(key)
                    self._update_busy()
                    self._bg_handlers[key](result, err, elapsed)
                elif msg[0] == "progress":
                    self.progress["value"] = msg[1] * 1000
                elif msg[0] == "done":
                    _, nrec, nerr, out = msg
                    self.progress["value"] = 1000
                    self.converting = False
                    self.btn_convert.configure(state="normal")
                    status = f"完了: {nrec:,} レコードを変換しました → {out}"
                    if nerr:
                        status += f" (変換エラー {nerr:,} 件 — #ERR表示)"
                        messagebox.showwarning(
                            "変換完了(警告あり)",
                            f"{nrec:,} レコードを変換しましたが、変換エラーが "
                            f"{nerr:,} 件ありました。\n出力中の #ERR:<16進> を確認し、"
                            "レイアウトを見直してください。")
                    else:
                        messagebox.showinfo("変換完了", f"{nrec:,} レコードを変換しました。\n{out}")
                    self.var_status.set(status)
                elif msg[0] == "error":
                    self.converting = False
                    self.btn_convert.configure(state="normal")
                    self.var_status.set(f"エラー: {msg[1]}")
                    messagebox.showerror("変換エラー", msg[1])
        except queue.Empty:
            pass
        self.after(100, self._poll_queue)


def main():
    # 高DPI対応 (Windows)
    try:
        import ctypes
        ctypes.windll.shcore.SetProcessDpiAwareness(1)
    except Exception:
        pass
    app = App()
    app.mainloop()


if __name__ == "__main__":
    main()
