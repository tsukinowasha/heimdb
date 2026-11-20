package store

import (
	"strings"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// キーの構成は docs/plan.md の「キー設計」を参照。
// 表記の m/clock などは、1 バイトのタグと順序を保つ符号化をした値をつないだもの。

const (
	tagMeta   byte = 'm'
	tagBranch byte = 'b'
	tagLog    byte = 'l' // l/<時刻>（コミットの記録。段階 24）

	tagClock   byte = 'c' // m/clock
	tagGC      byte = 'x' // m/gc
	tagSeq     byte = 's' // m/seq/<名前>
	tagBranchM byte = 'b' // m/branch/<ブランチID>
	tagMerge   byte = 'g' // m/merge/<ブランチID>/<反転時刻>
	tagResolve byte = 'r' // m/resolve/<ブランチID>/<テーブル名>/<id>
	tagUsers   byte = 'u' // m/users
	tagFormat  byte = 'f' // m/format
	tagLogTrim byte = 'l' // m/logtrim
	tagRewrite byte = 'w' // m/rewrite/<ブランチID>/<テーブルID>[/<インデックスID>]（段階 53）
	tagExclude byte = 'e' // m/mergeexclude（マージから外す表のパターン。段階 60）
	tagSync    byte = 'y' // m/sync/<ブランチID>/<反転時刻>（親の変更をブランチに取り込んだ記録。段階 94）

	tagCatalog byte = 'c' // b/<ブランチID>/c/<テーブル名>/<反転時刻>
	tagTable   byte = 't' // b/<ブランチID>/t/<テーブルID>/...
	tagAutoInc byte = 'a' // b/<ブランチID>/t/<テーブルID>/ai
	tagRow     byte = 'r' // b/<ブランチID>/t/<テーブルID>/r/<主キー...>/<反転時刻>
	tagIndex   byte = 'i' // b/<ブランチID>/t/<テーブルID>/i/<インデックスID>/<列...>/<主キー...>/<反転時刻>
)

// 値の先頭 1 バイト。
const (
	valTombstone byte = 0x00
	valData      byte = 0x01
)

// MainBranchID は main ブランチの ID。
const MainBranchID uint64 = 1

// 採番器の名前。
const (
	seqBranch = "branch"
	seqTable  = "table"
)

func clockKey() []byte {
	return []byte{tagMeta, tagClock}
}

func gcKey() []byte {
	return []byte{tagMeta, tagGC}
}

func formatKey() []byte {
	return []byte{tagMeta, tagFormat}
}

func logTrimKey() []byte {
	return []byte{tagMeta, tagLogTrim}
}

// rewriteKey は、書き直しの途中の新しいテーブル ID の印のキー（rewrite.go）。
func rewriteKey(branch, tableID uint64) []byte {
	return keyenc.AppendUint64(keyenc.AppendUint64([]byte{tagMeta, tagRewrite}, branch), tableID)
}

// indexBuildKey は、作成の途中のインデックス ID の印のキー（rewrite.go）。
func indexBuildKey(branch, tableID, indexID uint64) []byte {
	return keyenc.AppendUint64(rewriteKey(branch, tableID), indexID)
}

func mergeExcludeKey() []byte {
	return []byte{tagMeta, tagExclude}
}

func usersKey() []byte {
	return []byte{tagMeta, tagUsers}
}

func seqKey(name string) []byte {
	return keyenc.AppendString([]byte{tagMeta, tagSeq}, name)
}

func branchMetaKey(id uint64) []byte {
	return keyenc.AppendUint64([]byte{tagMeta, tagBranchM}, id)
}

func branchPrefix(branch uint64) []byte {
	return keyenc.AppendUint64([]byte{tagBranch}, branch)
}

func catalogPrefix(branch uint64) []byte {
	return append(branchPrefix(branch), tagCatalog)
}

// catalogNamePrefix は、あるテーブル名のすべてのバージョンに共通するキーの先頭部分。
// テーブル名は大文字と小文字を区別しないので、小文字にそろえる。
func catalogNamePrefix(branch uint64, name string) []byte {
	return keyenc.AppendString(catalogPrefix(branch), strings.ToLower(name))
}

func catalogKey(branch uint64, name string, ts uint64) []byte {
	return keyenc.AppendInvertedTime(catalogNamePrefix(branch, name), ts)
}

func tablePrefix(branch, tableID uint64) []byte {
	return keyenc.AppendUint64(append(branchPrefix(branch), tagTable), tableID)
}

func autoIncKey(branch, tableID uint64) []byte {
	return append(tablePrefix(branch, tableID), tagAutoInc)
}

// RowPrefix は、テーブルのすべての行のキーに共通する先頭部分。
func RowPrefix(branch, tableID uint64) []byte {
	return append(tablePrefix(branch, tableID), tagRow)
}

// rowKey は行のバージョンのキー。pk は rowenc.EncodePK で符号化した主キー。
func rowKey(branch, tableID uint64, pk []byte, ts uint64) []byte {
	return keyenc.AppendInvertedTime(append(RowPrefix(branch, tableID), pk...), ts)
}

// IndexPrefix は、インデックスのすべてのエントリのキーに共通する先頭部分。
func IndexPrefix(branch, tableID, indexID uint64) []byte {
	return keyenc.AppendUint64(append(tablePrefix(branch, tableID), tagIndex), indexID)
}

// indexKey はインデックスのエントリのバージョンのキー。entry は列の値と主キーを符号化したもの。
func indexKey(branch, tableID, indexID uint64, entry []byte, ts uint64) []byte {
	return keyenc.AppendInvertedTime(append(IndexPrefix(branch, tableID, indexID), entry...), ts)
}

// mergePrefix は、ブランチのマージの基準の記録に共通するキーの先頭部分。
func mergePrefix(branch uint64) []byte {
	return keyenc.AppendUint64([]byte{tagMeta, tagMerge}, branch)
}

func syncPrefix(branch uint64) []byte {
	return keyenc.AppendUint64([]byte{tagMeta, tagSync}, branch)
}

func resolvePrefix(branch uint64) []byte {
	return keyenc.AppendUint64([]byte{tagMeta, tagResolve}, branch)
}

func resolveKey(branch uint64, table, id string) []byte {
	return keyenc.AppendString(keyenc.AppendString(resolvePrefix(branch), table), id)
}
