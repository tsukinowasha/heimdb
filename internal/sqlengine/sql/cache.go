// Copyright 2020-2021 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: 照合順序の重みでハッシュする HashOfTypes を足す（docs/engine.md）

package sql

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"

	lru "github.com/hashicorp/golang-lru"
)

// HashOf returns a hash of the given value to be used as key in a cache.
func HashOf(ctx context.Context, v Row) (uint64, error) {
	hash := digestPool.Get().(*xxhash.Digest)
	hash.Reset()
	defer digestPool.Put(hash)
	for i, x := range v {
		if i > 0 {
			// separate each value in the row with a nil byte
			if _, err := hash.Write([]byte{0}); err != nil {
				return 0, err
			}
		}
		x, err := UnwrapAny(ctx, x)
		if err != nil {
			return 0, err
		}
		// TODO: probably much faster to do this with a type switch
		// TODO: we don't have the type info necessary to appropriately encode the value of a string with a non-standard
		//  collation, which means that two strings that differ only in their collations will hash to the same value.
		//  See rowexec/grouping_key()
		if _, err := fmt.Fprintf(hash, "%v,", x); err != nil {
			return 0, err
		}
	}
	return hash.Sum64(), nil
}

// HashOfTypes は HashOf と同じ。HeimDB: typs[i] が文字列の型（バイナリでない照合順序）の値は、照合順序の重みで
// ハッシュする（MySQL と同じく、照合順序で等しい値 'a'、'A'、'ａ' を同じにする。並べ替えや OrderedDistinct が使う
// Type().Compare と同じ照合順序）。DISTINCT、集合演算、COUNT(DISTINCT)、ハッシュ結合のキーなど、値の等しさで行を
// まとめるところで使う。行そのものを見分けるところ（UPDATE の行など）では使わない。
func HashOfTypes(ctx context.Context, v Row, typs []Type) (uint64, error) {
	hash := digestPool.Get().(*xxhash.Digest)
	hash.Reset()
	defer digestPool.Put(hash)
	for i, x := range v {
		if i > 0 {
			if _, err := hash.Write([]byte{0}); err != nil {
				return 0, err
			}
		}
		x, err := UnwrapAny(ctx, x)
		if err != nil {
			return 0, err
		}
		if i < len(typs) && writeCollatedHash(ctx, hash, typs[i], x) {
			continue
		}
		if _, err := fmt.Fprintf(hash, "%v,", x); err != nil {
			return 0, err
		}
	}
	return hash.Sum64(), nil
}

// writeCollatedHash は、文字列の型の値を照合順序の重みで書き、書いたかを返す（文字列でない値は型の字にしてから書く。
// NULL、バイナリの照合順序、壊れた UTF-8 は書かない。HashOf と同じ字で書く）。HeimDB。
func writeCollatedHash(ctx context.Context, hash *xxhash.Digest, typ Type, x any) bool {
	st, ok := typ.(StringType)
	if !ok || st.Collation() == Collation_binary || x == nil {
		return false
	}
	var s string
	switch v := x.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		// 型の字にしてから書く（IF(c, 文字列の列, 数の列) は枝の値を変換しないので、'1' と 1 が来る。GROUP BY の
		// キーと同じく、型の文字列にそろえる）
		c, _, err := typ.Convert(ctx, x)
		if err != nil {
			return false
		}
		switch c := c.(type) {
		case string:
			s = c
		case []byte:
			s = string(c)
		default:
			return false
		}
	}
	if !utf8.ValidString(s) {
		return false
	}
	// 重みの前に印を置き、文字列でない値の字と混ざらないようにする
	if _, err := hash.Write([]byte{1}); err != nil {
		return false
	}
	return st.Collation().WriteWeightString(hash, s) == nil
}

// TypesOf は、スキーマの列の型。HeimDB: HashOfTypes に渡す。
func TypesOf(sch Schema) []Type {
	typs := make([]Type, len(sch))
	for i, c := range sch {
		typs[i] = c.Type
	}
	return typs
}

var digestPool = sync.Pool{
	New: func() any {
		return xxhash.New()
	},
}

// ErrKeyNotFound is returned when the key could not be found in the cache.
var ErrKeyNotFound = fmt.Errorf("memory: key not found in cache")

type lruCache struct {
	memory   Freeable
	reporter Reporter
	size     int
	cache    *lru.Cache
}

func (l *lruCache) Size() int {
	return l.size
}

func newLRUCache(memory Freeable, r Reporter, size uint) *lruCache {
	lru, _ := lru.New(int(size))
	return &lruCache{memory, r, int(size), lru}
}

func (l *lruCache) Put(k uint64, v interface{}) error {
	if releaseMemoryIfNeeded(l.reporter, l.Free, l.memory.Free) {
		l.cache.Add(k, v)
	}
	return nil
}

func (l *lruCache) Get(k uint64) (interface{}, error) {
	v, ok := l.cache.Get(k)
	if !ok {
		return nil, ErrKeyNotFound
	}

	return v, nil
}

func (l *lruCache) Free() {
	l.cache, _ = lru.New(l.size)
}

func (l *lruCache) Dispose() {
	l.memory = nil
	l.cache = nil
}

type rowsCache struct {
	memory   Freeable
	reporter Reporter
	rows     []Row
	rows2    []Row2
}

func newRowsCache(memory Freeable, r Reporter) *rowsCache {
	return &rowsCache{memory: memory, reporter: r}
}

func (c *rowsCache) Add(row Row) error {
	if !releaseMemoryIfNeeded(c.reporter, c.memory.Free) {
		return ErrNoMemoryAvailable.New()
	}

	c.rows = append(c.rows, row)
	return nil
}

func (c *rowsCache) Get() []Row { return c.rows }

func (c *rowsCache) Add2(row2 Row2) error {
	if !releaseMemoryIfNeeded(c.reporter, c.memory.Free) {
		return ErrNoMemoryAvailable.New()
	}

	c.rows2 = append(c.rows2, row2)
	return nil
}

func (c *rowsCache) Get2() []Row2 {
	return c.rows2
}

func (c *rowsCache) Dispose() {
	c.memory = nil
	c.rows = nil
}

// mapCache is a simple in-memory implementation of a cache
type mapCache struct {
	cache map[uint64]interface{}
}

func (m mapCache) Put(u uint64, i interface{}) error {
	m.cache[u] = i
	return nil
}

func (m mapCache) Get(u uint64) (interface{}, error) {
	v, ok := m.cache[u]
	if !ok {
		return nil, ErrKeyNotFound
	}
	return v, nil
}

func (m mapCache) Size() int {
	return len(m.cache)
}

func NewMapCache() mapCache {
	return mapCache{
		cache: make(map[uint64]interface{}),
	}
}

type historyCache struct {
	memory   Freeable
	reporter Reporter
	cache    map[uint64]interface{}
}

func (h *historyCache) Size() int {
	return len(h.cache)
}

func newHistoryCache(memory Freeable, r Reporter) *historyCache {
	return &historyCache{memory, r, make(map[uint64]interface{})}
}

func (h *historyCache) Put(k uint64, v interface{}) error {
	if !releaseMemoryIfNeeded(h.reporter, h.memory.Free) {
		return ErrNoMemoryAvailable.New()
	}
	h.cache[k] = v
	return nil
}

func (h *historyCache) Get(k uint64) (interface{}, error) {
	v, ok := h.cache[k]
	if !ok {
		return nil, ErrKeyNotFound
	}
	return v, nil
}

func (h *historyCache) Dispose() {
	h.memory = nil
	h.cache = nil
}

// releasesMemoryIfNeeded releases memory if needed using the following steps
// until there is available memory. It returns whether or not there was
// available memory after all the steps.
func releaseMemoryIfNeeded(r Reporter, steps ...func()) bool {
	for _, s := range steps {
		if HasAvailableMemory(r) {
			return true
		}

		s()
		runtime.GC()
	}

	return HasAvailableMemory(r)
}
