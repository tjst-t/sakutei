// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build integration_redfish

package redfish

import (
	"encoding/binary"
	"strings"
)

// 仮想メディアに差す小さな ISO を作る。ホストにも qemu-bmc のイメージにも
// xorriso などが無いので、空のルートディレクトリだけを持つ ISO 9660 を
// その場で書く（21 セクタ = 42 KiB）。
//
//	0〜15  システム領域（0）
//	16     Primary Volume Descriptor
//	17     Volume Descriptor Set Terminator
//	18 / 19 パステーブル（L 型 / M 型）
//	20     ルートディレクトリ（"." と ".."）

const (
	isoSector    = 2048
	isoPVD       = 16
	isoTerm      = 17
	isoLPath     = 18
	isoMPath     = 19
	isoRoot      = 20
	isoSectors   = 21
	isoPathTable = 10 // ルートだけのパステーブルの大きさ
)

// makeISO は volumeID（大文字・数字・_、32 字まで）の空の ISO 9660 を返す。
func makeISO(volumeID string) []byte {
	img := make([]byte, isoSectors*isoSector)

	pvd := img[isoPVD*isoSector : (isoPVD+1)*isoSector]
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], padSpaces("LINUX", 32))
	copy(pvd[40:72], padSpaces(volumeID, 32))
	both32(pvd[80:88], isoSectors)
	both16(pvd[120:124], 1) // volume set size
	both16(pvd[124:128], 1) // volume sequence number
	both16(pvd[128:132], isoSector)
	both32(pvd[132:140], isoPathTable)
	binary.LittleEndian.PutUint32(pvd[140:144], isoLPath)
	binary.BigEndian.PutUint32(pvd[148:152], isoMPath)
	dirRecord(pvd[156:190], 0)
	for _, f := range [][2]int{{190, 318}, {318, 446}, {446, 574}, {574, 702}, {702, 739}, {739, 776}, {776, 813}} {
		copy(pvd[f[0]:f[1]], padSpaces("", f[1]-f[0]))
	}
	for _, off := range []int{813, 830, 847, 864} {
		copy(pvd[off:off+16], "0000000000000000") // 日時は「指定なし」
	}
	pvd[881] = 1 // file structure version

	term := img[isoTerm*isoSector : (isoTerm+1)*isoSector]
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1

	pathEntry := func(b []byte, put func([]byte, uint32), put16 func([]byte, uint16)) {
		b[0] = 1 // 名前の長さ
		put(b[2:6], isoRoot)
		put16(b[6:8], 1) // 親ディレクトリの番号
	}
	pathEntry(img[isoLPath*isoSector:], binary.LittleEndian.PutUint32, binary.LittleEndian.PutUint16)
	pathEntry(img[isoMPath*isoSector:], binary.BigEndian.PutUint32, binary.BigEndian.PutUint16)

	root := img[isoRoot*isoSector:]
	dirRecord(root[0:34], 0)  // "."
	dirRecord(root[34:68], 1) // ".."
	return img
}

// dirRecord はルートディレクトリを指す 34 バイトのディレクトリレコード（名前は
// 1 バイトの name: 0 = "."、1 = ".."）を b に書く。
func dirRecord(b []byte, name byte) {
	b[0] = 34
	both32(b[2:10], isoRoot)
	both32(b[10:18], isoSector)
	b[25] = 2 // ディレクトリ
	both16(b[28:32], 1)
	b[32] = 1
	b[33] = name
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b[0:4], v)
	binary.BigEndian.PutUint32(b[4:8], v)
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b[0:2], v)
	binary.BigEndian.PutUint16(b[2:4], v)
}

func padSpaces(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}
