// mksyso — Windows マニフェストを埋め込む COFF オブジェクト (.syso) を生成する
//
// Walk は Common Controls v6 を要求するため、exe にマニフェストが必要。
// go build は同じディレクトリの .syso を自動でリンクする。
//
// 使い方: go run ./tools/mksyso <manifest> <出力.syso>
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

const (
	machineAMD64   = 0x8664
	rtManifest     = 24
	manifestID     = 1
	langEnUS       = 0x0409
	dirEntryIsDir  = 0x80000000
	sectionRsrc    = 0x40000040 // INITIALIZED_DATA | MEM_READ
	relAddr32NB    = 3
	symClassStatic = 3

	fileHeaderSize    = 20
	sectionHeaderSize = 40
	resDirSize        = 16 + 8 // 見出し + エントリ1個
	dataEntryOffset   = resDirSize * 3
	dataOffset        = dataEntryOffset + 16
)

func le(buf *bytes.Buffer, v ...any) {
	for _, x := range v {
		binary.Write(buf, binary.LittleEndian, x)
	}
}

// resourceDir はエントリ1個のリソースディレクトリを書く。
func resourceDir(buf *bytes.Buffer, id, target uint32) {
	le(buf, uint32(0), uint32(0), uint16(0), uint16(0), uint16(0), uint16(1), id, target)
}

func build(manifest []byte) []byte {
	var rsrc bytes.Buffer
	resourceDir(&rsrc, rtManifest, dirEntryIsDir|resDirSize)
	resourceDir(&rsrc, manifestID, dirEntryIsDir|resDirSize*2)
	resourceDir(&rsrc, langEnUS, dataEntryOffset)
	le(&rsrc, uint32(dataOffset), uint32(len(manifest)), uint32(0), uint32(0))
	rsrc.Write(manifest)
	for rsrc.Len()%8 != 0 {
		rsrc.WriteByte(0)
	}

	rawOffset := uint32(fileHeaderSize + sectionHeaderSize)
	relocOffset := rawOffset + uint32(rsrc.Len())
	symOffset := relocOffset + 10

	var out bytes.Buffer
	le(&out, uint16(machineAMD64), uint16(1), uint32(0), symOffset, uint32(1), uint16(0), uint16(0))
	out.WriteString(".rsrc\x00\x00\x00")
	le(&out, uint32(0), uint32(0), uint32(rsrc.Len()), rawOffset, relocOffset, uint32(0),
		uint16(1), uint16(0), uint32(sectionRsrc))
	out.Write(rsrc.Bytes())
	// データエントリの RVA をリンク時に確定させる
	le(&out, uint32(dataEntryOffset), uint32(0), uint16(relAddr32NB))
	out.WriteString(".rsrc\x00\x00\x00")
	le(&out, uint32(0), int16(1), uint16(0), uint8(symClassStatic), uint8(0))
	le(&out, uint32(4)) // 文字列テーブル (空)
	return out.Bytes()
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "使い方: mksyso <manifest> <出力.syso>")
		os.Exit(2)
	}
	manifest, err := os.ReadFile(os.Args[1])
	if err == nil {
		err = os.WriteFile(os.Args[2], build(manifest), 0o666)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
