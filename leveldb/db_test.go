// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/syndtr/goleveldb/leveldb/cache"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

func tkey(i int) []byte {
	return []byte(fmt.Sprintf("%016d", i))
}

func tval(seed, n int) []byte {
	r := rand.New(rand.NewSource(int64(seed)))
	return randomString(r, n)
}

type dbHarness struct {
	t *testing.T

	stor *testingStorage
	db   *DB
	o    *opt.Options
	oo   opt.OptionsSetter
	ro   *opt.ReadOptions
	wo   *opt.WriteOptions
}

func newDbHarnessWoptRaw(t *testing.T, o *opt.Options) *dbHarness {
	h := new(dbHarness)
	h.init(t, o)
	return h
}

func newDbHarnessWopt(t *testing.T, o *opt.Options) *dbHarness {
	o.Flag |= opt.OFCreateIfMissing
	return newDbHarnessWoptRaw(t, o)
}

func newDbHarness(t *testing.T) *dbHarness {
	return newDbHarnessWopt(t, &opt.Options{})
}

func (h *dbHarness) init(t *testing.T, o *opt.Options) {
	h.t = t
	h.stor = newTestingStorage(t)
	h.o = o
	h.ro = &opt.ReadOptions{}
	h.wo = &opt.WriteOptions{}

	h.openDB()
}

func (h *dbHarness) openDB() {
	var err error
	h.db, err = Open(h.stor, h.o)
	if err != nil {
		h.t.Fatal("Open: got error: ", err)
	}
	h.oo = h.db.GetOptionsSetter()
}

func (h *dbHarness) closeDB() {
	err := h.db.Close()
	if err != nil {
		h.t.Error("Close: got error: ", err)
	}
	runtime.GC()
}

func (h *dbHarness) reopenDB() {
	h.closeDB()
	h.openDB()
}

func (h *dbHarness) close() {
	h.closeDB()
	h.o = nil
	h.db = nil
	h.stor = nil
	runtime.GC()
}

func (h *dbHarness) openAssert(want bool) {
	db, err := Open(h.stor, h.o)
	if err != nil {
		if want {
			h.t.Error("Open: assert: got error: ", err)
		} else {
			h.t.Log("Open: assert: got error (expected): ", err)
		}
	} else {
		if !want {
			h.t.Error("Open: assert: expect error")
		}
		db.Close()
	}
}

func (h *dbHarness) put(key, value string) {
	t := h.t
	db := h.db

	err := db.Put([]byte(key), []byte(value), h.wo)
	if err != nil {
		t.Error("Put: got error: ", err)
	}
}

func (h *dbHarness) putMulti(n int, low, hi string) {
	for i := 0; i < n; i++ {
		h.put(low, "begin")
		h.put(hi, "end")
		h.compactMem()
	}
}

func (h *dbHarness) maxNextLevelOverlappingBytes(want uint64) {
	t := h.t
	db := h.db

	var res uint64
	ucmp := db.s.cmp.cmp
	v := db.s.version()
	for i, tt := range v.tables[1 : len(v.tables)-1] {
		level := i + 1
		next := v.tables[level+1]
		for _, t := range tt {
			var r tFiles
			min, max := t.min.ukey(), t.max.ukey()
			next.getOverlaps(min, max, &r, true, ucmp)
			sum := r.size()
			if sum > res {
				res = sum
			}
		}
	}

	if res > want {
		t.Errorf("next level overlapping bytes is more than %d, got=%d", want, res)
	}
}

func (h *dbHarness) delete(key string) {
	t := h.t
	db := h.db

	err := db.Delete([]byte(key), h.wo)
	if err != nil {
		t.Error("Delete: got error: ", err)
	}
}

func (h *dbHarness) getr(db Reader, key string, expectFound bool) (found bool, v []byte) {
	t := h.t
	v, err := db.Get([]byte(key), h.ro)
	switch err {
	case errors.ErrNotFound:
		if expectFound {
			t.Errorf("Get: key '%s' not found, want found", key)
		}
	case nil:
		found = true
		if !expectFound {
			t.Errorf("Get: key '%s' found, want not found", key)
		}
	default:
		t.Error("Get: got error: ", err)
	}
	return
}

func (h *dbHarness) get(key string, expectFound bool) (found bool, v []byte) {
	return h.getr(h.db, key, expectFound)
}

func (h *dbHarness) getValr(db Reader, key, value string) {
	t := h.t
	found, r := h.getr(db, key, true)
	if !found {
		return
	}
	rval := string(r)
	if rval != value {
		t.Errorf("Get: invalid value, got '%s', want '%s'", rval, value)
	}
}

func (h *dbHarness) getVal(key, value string) {
	h.getValr(h.db, key, value)
}

func (h *dbHarness) allEntriesFor(key, want string) {
	t := h.t
	db := h.db
	ucmp := db.s.cmp.cmp

	ikey := newIKey([]byte(key), kMaxSeq, tVal)
	iter := db.newRawIterator(new(opt.ReadOptions))
	if !iter.Seek(ikey) && iter.Error() != nil {
		t.Error("AllEntries: error during seek, err: ", iter.Error())
		return
	}
	res := "[ "
	first := true
	for iter.Valid() {
		rkey := iKey(iter.Key())
		if _, t, ok := rkey.parseNum(); ok {
			if ucmp.Compare(ikey.ukey(), rkey.ukey()) != 0 {
				break
			}
			if !first {
				res += ", "
			}
			first = false
			switch t {
			case tVal:
				res += string(iter.Value())
			case tDel:
				res += "DEL"
			}
		} else {
			if !first {
				res += ", "
			}
			first = false
			res += "CORRUPTED"
		}
		iter.Next()
	}
	if !first {
		res += " "
	}
	res += "]"
	if res != want {
		t.Errorf("AllEntries: assert failed for key %q, got=%q want=%q", key, res, want)
	}
}

// Return a string that contains all key,value pairs in order,
// formatted like "(k1->v1)(k2->v2)".
func (h *dbHarness) getKeyVal(want string) {
	t := h.t
	db := h.db

	s, err := db.GetSnapshot()
	if err != nil {
		t.Fatal("GetSnapshot: got error: ", err)
	}
	res := ""
	iter := s.NewIterator(new(opt.ReadOptions))
	for iter.Next() {
		res += fmt.Sprintf("(%s->%s)", string(iter.Key()), string(iter.Value()))
	}

	if res != want {
		t.Errorf("GetKeyVal: invalid key/value pair, got=%q want=%q", res, want)
	}
	s.Release()
}

func (h *dbHarness) compactMem() {
	t := h.t
	db := h.db

	db.cch <- cSched

	if db.getMem().cur.Len() == 0 {
		return
	}

	// create new memdb and journal
	_, err := db.newMem()
	if err != nil {
		t.Error("newMem: got error: ", err)
		return
	}

	// schedule compaction
	select {
	case db.cch <- cSched:
	default:
	}
	db.cch <- cWait

	if h.totalTables() == 0 {
		t.Error("zero tables after mem compaction")
	}
}

func (h *dbHarness) compactRangeAtErr(level int, min, max string, wanterr bool) {
	t := h.t
	db := h.db

	r := new(cReq)
	r.level = level
	if min != "" {
		r.min = []byte(min)
	}
	if max != "" {
		r.max = []byte(max)
	}

	db.creq <- r
	db.cch <- cWait

	err := db.wok()
	if err != nil {
		if wanterr {
			t.Log("CompactRangeAt: got error (expected): ", err)
		} else {
			t.Error("CompactRangeAt: got error: ", err)
		}
	} else if wanterr {
		t.Error("CompactRangeAt: expect error")
	}
}

func (h *dbHarness) compactRangeAt(level int, min, max string) {
	h.compactRangeAtErr(level, min, max, false)
}

func (h *dbHarness) compactRange(min, max string) {
	t := h.t
	db := h.db

	var r Range
	if min != "" {
		r.Start = []byte(min)
	}
	if max != "" {
		r.Limit = []byte(max)
	}
	err := db.CompactRange(r)
	if err != nil {
		t.Error("CompactRange: got error: ", err)
	}
}

func (h *dbHarness) sizeAssert(start, limit string, low, hi uint64) {
	t := h.t
	db := h.db

	s, err := db.GetApproximateSizes([]Range{
		Range{[]byte(start), []byte(limit)},
	})
	if err != nil {
		t.Error("GetApproximateSizes: got error: ", err)
	}
	if s.Sum() < low || s.Sum() > hi {
		t.Errorf("sizeof %q to %q not in range, want %d - %d, got %d",
			shorten(start), shorten(limit), low, hi, s.Sum())
	}
}

func (h *dbHarness) getSnapshot() (s *Snapshot) {
	s, err := h.db.GetSnapshot()
	if err != nil {
		h.t.Fatal("GetSnapshot: got error: ", err)
	}
	return
}
func (h *dbHarness) tablesPerLevel(want string) {
	res := ""
	nz := 0
	v := h.db.s.version()
	for level, tt := range v.tables {
		if level > 0 {
			res += ","
		}
		res += fmt.Sprint(len(tt))
		if len(tt) > 0 {
			nz = len(res)
		}
	}
	res = res[:nz]
	if res != want {
		h.t.Errorf("invalid tables len, want=%s, got=%s", want, res)
	}
}

func (h *dbHarness) totalTables() (n int) {
	v := h.db.s.version()
	for _, tt := range v.tables {
		n += len(tt)
	}
	return
}

type keyValue interface {
	Key() []byte
	Value() []byte
}

func testKeyVal(t *testing.T, kv keyValue, want string) {
	res := string(kv.Key()) + "->" + string(kv.Value())
	if res != want {
		t.Errorf("invalid key/value, want=%q, got=%q", want, res)
	}
}

func numKey(num int) string {
	return fmt.Sprintf("key%06d", num)
}

var _bloom_filter = filter.NewBloomFilter(10)

func runAllOpts(t *testing.T, f func(h *dbHarness)) {
	for i := 0; i < 4; i++ {
		h := newDbHarness(t)
		switch i {
		case 0:
		case 1:
			h.oo.SetFilter(_bloom_filter)
		case 2:
			h.oo.SetCompressionType(opt.NoCompression)
		case 3:
			h.reopenDB()
		}
		f(h)
		h.close()
	}
}

func testAligned(t *testing.T, name string, offset uintptr) {
	if offset%8 != 0 {
		t.Errorf("field %s offset is not 64-bit aligned", name)
	}
}

func Test_FieldsAligned(t *testing.T) {
	p1 := new(DB)
	testAligned(t, "DB.seq", unsafe.Offsetof(p1.seq))
	testAligned(t, "DB.fseq", unsafe.Offsetof(p1.fseq))
	p2 := new(session)
	testAligned(t, "session.stFileNum", unsafe.Offsetof(p2.stFileNum))
	testAligned(t, "session.stJournalNum", unsafe.Offsetof(p2.stJournalNum))
	testAligned(t, "session.stPrevJournalNum", unsafe.Offsetof(p2.stPrevJournalNum))
	testAligned(t, "session.stSeq", unsafe.Offsetof(p2.stSeq))
}

func TestDb_Locking(t *testing.T) {
	h := newDbHarness(t)
	h.openAssert(false)
	h.closeDB()
	h.openAssert(true)
}

func TestDb_Empty(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.get("foo", false)

		h.reopenDB()
		h.get("foo", false)
	})
}

func TestDb_ReadWrite(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.put("foo", "v1")
		h.getVal("foo", "v1")
		h.put("bar", "v2")
		h.put("foo", "v3")
		h.getVal("foo", "v3")
		h.getVal("bar", "v2")

		h.reopenDB()
		h.getVal("foo", "v3")
		h.getVal("bar", "v2")
	})
}

func TestDb_PutDeleteGet(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.put("foo", "v1")
		h.getVal("foo", "v1")
		h.put("foo", "v2")
		h.getVal("foo", "v2")
		h.delete("foo")
		h.get("foo", false)

		h.reopenDB()
		h.get("foo", false)
	})
}

func TestDb_EmptyBatch(t *testing.T) {
	h := newDbHarness(t)
	h.get("foo", false)

	err := h.db.Write(new(Batch), h.wo)
	if err != nil {
		t.Error("writing empty batch yield error: ", err)
	}
	h.get("foo", false)

	h.close()
}

func TestDb_GetFromFrozen(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{WriteBuffer: 100000})

	h.put("foo", "v1")
	h.getVal("foo", "v1")

	h.stor.DelaySync(storage.TypeTable)      // Block sync calls
	h.put("k1", strings.Repeat("x", 100000)) // Fill memtable
	h.put("k2", strings.Repeat("y", 100000)) // Trigger compaction
	h.getVal("foo", "v1")
	h.stor.ReleaseSync(storage.TypeTable) // Release sync calls

	h.reopenDB()
	h.getVal("foo", "v1")
	h.get("k1", true)
	h.get("k2", true)

	h.close()
}

func TestDb_GetFromTable(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.put("foo", "v1")
		h.compactMem()
		h.getVal("foo", "v1")
	})
}

func TestDb_GetSnapshot(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		bar := strings.Repeat("b", 200)
		h.put("foo", "v1")
		h.put(bar, "v1")

		snap, err := h.db.GetSnapshot()
		if err != nil {
			t.Fatal("GetSnapshot: got error: ", err)
		}

		h.put("foo", "v2")
		h.put(bar, "v2")

		h.getVal("foo", "v2")
		h.getVal(bar, "v2")
		h.getValr(snap, "foo", "v1")
		h.getValr(snap, bar, "v1")

		h.compactMem()

		h.getVal("foo", "v2")
		h.getVal(bar, "v2")
		h.getValr(snap, "foo", "v1")
		h.getValr(snap, bar, "v1")

		snap.Release()

		h.reopenDB()
		h.getVal("foo", "v2")
		h.getVal(bar, "v2")
	})
}

func TestDb_GetLevel0Ordering(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		for i := 0; i < 4; i++ {
			h.put("bar", fmt.Sprintf("b%d", i))
			h.put("foo", fmt.Sprintf("v%d", i))
			h.compactMem()
		}
		h.getVal("foo", "v3")
		h.getVal("bar", "b3")

		t0len := h.db.s.version().tLen(0)
		if t0len < 2 {
			t.Errorf("level-0 tables is less than 2, got %d", t0len)
		}

		h.reopenDB()
		h.getVal("foo", "v3")
		h.getVal("bar", "b3")
	})
}

func TestDb_GetOrderedByLevels(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.put("foo", "v1")
		h.compactMem()
		h.compactRange("a", "z")
		h.getVal("foo", "v1")
		h.put("foo", "v2")
		h.compactMem()
		h.getVal("foo", "v2")
	})
}

func TestDb_GetPicksCorrectFile(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		// Arrange to have multiple files in a non-level-0 level.
		h.put("a", "va")
		h.compactMem()
		h.compactRange("a", "b")
		h.put("x", "vx")
		h.compactMem()
		h.compactRange("x", "y")
		h.put("f", "vf")
		h.compactMem()
		h.compactRange("f", "g")

		h.getVal("a", "va")
		h.getVal("f", "vf")
		h.getVal("x", "vx")

		h.compactRange("", "")
		h.getVal("a", "va")
		h.getVal("f", "vf")
		h.getVal("x", "vx")
	})
}

func TestDb_GetEncountersEmptyLevel(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		// Arrange for the following to happen:
		//   * sstable A in level 0
		//   * nothing in level 1
		//   * sstable B in level 2
		// Then do enough Get() calls to arrange for an automatic compaction
		// of sstable A.  A bug would cause the compaction to be marked as
		// occuring at level 1 (instead of the correct level 0).

		// Step 1: First place sstables in levels 0 and 2
		for i := 0; ; i++ {
			if i >= 100 {
				t.Fatal("could not fill levels-0 and level-2")
			}
			v := h.db.s.version()
			if v.tLen(0) > 0 && v.tLen(2) > 0 {
				break
			}
			h.put("a", "begin")
			h.put("z", "end")
			h.compactMem()

			h.getVal("a", "begin")
			h.getVal("z", "end")
		}

		// Step 2: clear level 1 if necessary.
		h.compactRangeAt(1, "", "")
		h.tablesPerLevel("1,0,1")

		h.getVal("a", "begin")
		h.getVal("z", "end")

		// Step 3: read a bunch of times
		for i := 0; i < 200; i++ {
			h.get("missing", false)
		}

		// Step 4: Wait for compaction to finish
		h.db.cch <- cWait

		v := h.db.s.version()
		if v.tLen(0) > 0 {
			t.Errorf("level-0 tables more than 0, got %d", v.tLen(0))
		}

		h.getVal("a", "begin")
		h.getVal("z", "end")
	})
}

func TestDb_IterMultiWithDelete(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.put("a", "va")
		h.put("b", "vb")
		h.put("c", "vc")
		h.delete("b")
		h.get("b", false)

		iter := h.db.NewIterator(new(opt.ReadOptions))
		iter.Seek([]byte("c"))
		testKeyVal(t, iter, "c->vc")
		iter.Prev()
		testKeyVal(t, iter, "a->va")

		h.compactMem()

		iter = h.db.NewIterator(new(opt.ReadOptions))
		iter.Seek([]byte("c"))
		testKeyVal(t, iter, "c->vc")
		iter.Prev()
		testKeyVal(t, iter, "a->va")
	})
}

func TestDb_IteratorPinsRef(t *testing.T) {
	h := newDbHarness(t)

	h.put("foo", "hello")

	// Get iterator that will yield the current contents of the DB.
	iter := h.db.NewIterator(new(opt.ReadOptions))

	// Write to force compactions
	h.put("foo", "newvalue1")
	for i := 0; i < 100; i++ {
		h.put(numKey(i), strings.Repeat(fmt.Sprintf("v%09d", i), 100000/10))
	}
	h.put("foo", "newvalue2")

	iter.First()
	testKeyVal(t, iter, "foo->hello")
	if iter.Next() {
		t.Errorf("expect eof")
	}

	h.close()
}

func TestDb_Recover(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.put("foo", "v1")
		h.put("baz", "v5")

		h.reopenDB()
		h.getVal("foo", "v1")

		h.getVal("foo", "v1")
		h.getVal("baz", "v5")
		h.put("bar", "v2")
		h.put("foo", "v3")

		h.reopenDB()
		h.getVal("foo", "v3")
		h.put("foo", "v4")
		h.getVal("foo", "v4")
		h.getVal("bar", "v2")
		h.getVal("baz", "v5")
	})
}

func TestDb_RecoverWithEmptyJournal(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.put("foo", "v1")
		h.put("foo", "v2")

		h.reopenDB()
		h.reopenDB()
		h.put("foo", "v3")

		h.reopenDB()
		h.getVal("foo", "v3")
	})
}

func TestDb_RecoverDuringMemtableCompaction(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.oo.SetWriteBuffer(1000000)

		h.stor.DelaySync(storage.TypeTable)
		h.put("foo", "v1")
		h.put("big1", strings.Repeat("x", 10000000))
		h.put("big2", strings.Repeat("y", 1000))
		h.put("bar", "v2")
		h.stor.ReleaseSync(storage.TypeTable)

		h.reopenDB()
		h.getVal("foo", "v1")
		h.getVal("bar", "v2")
		h.getVal("big1", strings.Repeat("x", 10000000))
		h.getVal("big2", strings.Repeat("y", 1000))
	})
}

func TestDb_MinorCompactionsHappen(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{WriteBuffer: 10000})

	n := 500

	key := func(i int) string {
		return fmt.Sprintf("key%06d", i)
	}

	for i := 0; i < n; i++ {
		h.put(key(i), key(i)+strings.Repeat("v", 1000))
	}

	for i := 0; i < n; i++ {
		h.getVal(key(i), key(i)+strings.Repeat("v", 1000))
	}

	h.reopenDB()
	for i := 0; i < n; i++ {
		h.getVal(key(i), key(i)+strings.Repeat("v", 1000))
	}

	h.close()
}

func TestDb_RecoverWithLargeJournal(t *testing.T) {
	h := newDbHarness(t)

	h.put("big1", strings.Repeat("1", 200000))
	h.put("big2", strings.Repeat("2", 200000))
	h.put("small3", strings.Repeat("3", 10))
	h.put("small4", strings.Repeat("4", 10))
	h.tablesPerLevel("")

	// Make sure that if we re-open with a small write buffer size that
	// we flush table files in the middle of a large journal file.
	h.o.WriteBuffer = 100000
	h.reopenDB()
	h.getVal("big1", strings.Repeat("1", 200000))
	h.getVal("big2", strings.Repeat("2", 200000))
	h.getVal("small3", strings.Repeat("3", 10))
	h.getVal("small4", strings.Repeat("4", 10))
	if h.db.s.version().tLen(0) <= 1 {
		t.Errorf("tables-0 less than one")
	}

	h.close()
}

func TestDb_CompactionsGenerateMultipleFiles(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{
		WriteBuffer:     100000000,
		CompressionType: opt.NoCompression,
	})

	v := h.db.s.version()
	if v.tLen(0) > 0 {
		t.Errorf("level-0 tables more than 0, got %d", v.tLen(0))
	}

	n := 80

	// Write 8MB (80 values, each 100K)
	for i := 0; i < n; i++ {
		h.put(numKey(i), strings.Repeat(fmt.Sprintf("v%09d", i), 100000/10))
	}

	// Reopening moves updates to level-0
	h.reopenDB()
	h.compactRangeAt(0, "", "")

	v = h.db.s.version()
	if v.tLen(0) > 0 {
		t.Errorf("level-0 tables more than 0, got %d", v.tLen(0))
	}
	if v.tLen(1) <= 1 {
		t.Errorf("level-1 tables less than 1, got %d", v.tLen(1))
	}

	for i := 0; i < n; i++ {
		h.getVal(numKey(i), strings.Repeat(fmt.Sprintf("v%09d", i), 100000/10))
	}

	h.close()
}

func TestDb_RepeatedWritesToSameKey(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{WriteBuffer: 100000})

	maxTables := kNumLevels + kL0_StopWritesTrigger

	value := strings.Repeat("v", 2*h.o.WriteBuffer)
	for i := 0; i < 5*maxTables; i++ {
		h.put("key", value)
		n := h.totalTables()
		if n > maxTables {
			t.Errorf("total tables exceed %d, got=%d, iter=%d", maxTables, n, i)
		}
	}

	h.close()
}

func TestDb_RepeatedWritesToSameKeyAfterReopen(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{WriteBuffer: 100000})

	h.reopenDB()

	maxTables := kNumLevels + kL0_StopWritesTrigger

	value := strings.Repeat("v", 2*h.o.WriteBuffer)
	for i := 0; i < 5*maxTables; i++ {
		h.put("key", value)
		n := h.totalTables()
		if n > maxTables {
			t.Errorf("total tables exceed %d, got=%d, iter=%d", maxTables, n, i)
		}
	}

	h.close()
}

func TestDb_SparseMerge(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{CompressionType: opt.NoCompression})

	h.putMulti(kNumLevels, "A", "Z")

	// Suppose there is:
	//    small amount of data with prefix A
	//    large amount of data with prefix B
	//    small amount of data with prefix C
	// and that recent updates have made small changes to all three prefixes.
	// Check that we do not do a compaction that merges all of B in one shot.
	h.put("A", "va")
	value := strings.Repeat("x", 1000)
	for i := 0; i < 100000; i++ {
		h.put(fmt.Sprintf("B%010d", i), value)
	}
	h.put("C", "vc")
	h.compactMem()
	h.compactRangeAt(0, "", "")

	// Make sparse update
	h.put("A", "va2")
	h.put("B100", "bvalue2")
	h.put("C", "vc2")
	h.compactMem()

	h.maxNextLevelOverlappingBytes(20 * 1048576)
	h.compactRangeAt(0, "", "")
	h.maxNextLevelOverlappingBytes(20 * 1048576)
	h.compactRangeAt(1, "", "")
	h.maxNextLevelOverlappingBytes(20 * 1048576)

	h.close()
}

func TestDb_ApproximateSizes(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{
		CompressionType: opt.NoCompression,
		WriteBuffer:     100000000,
	})

	h.sizeAssert("", "xyz", 0, 0)
	h.reopenDB()
	h.sizeAssert("", "xyz", 0, 0)

	// Write 8MB (80 values, each 100K)
	n := 80
	s1 := 100000
	s2 := 105000

	for i := 0; i < n; i++ {
		h.put(numKey(i), strings.Repeat(fmt.Sprintf("v%09d", i), s1/10))
	}

	// 0 because GetApproximateSizes() does not account for memtable space
	h.sizeAssert("", numKey(50), 0, 0)

	for r := 0; r < 3; r++ {
		h.reopenDB()

		for cs := 0; cs < n; cs += 10 {
			for i := 0; i < n; i += 10 {
				h.sizeAssert("", numKey(i), uint64(s1*i), uint64(s2*i))
				h.sizeAssert("", numKey(i)+".suffix", uint64(s1*(i+1)), uint64(s2*(i+1)))
				h.sizeAssert(numKey(i), numKey(i+10), uint64(s1*10), uint64(s2*10))
			}

			h.sizeAssert("", numKey(50), uint64(s1*50), uint64(s2*50))
			h.sizeAssert("", numKey(50)+".suffix", uint64(s1*50), uint64(s2*50))

			h.compactRangeAt(0, numKey(cs), numKey(cs+9))
		}

		v := h.db.s.version()
		if v.tLen(0) != 0 {
			t.Errorf("level-0 tables was not zero, got %d", v.tLen(0))
		}
		if v.tLen(1) == 0 {
			t.Error("level-1 tables was zero")
		}
	}

	h.close()
}

func TestDb_ApproximateSizes_MixOfSmallAndLarge(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{CompressionType: opt.NoCompression})

	sizes := []uint64{
		10000,
		10000,
		100000,
		10000,
		100000,
		10000,
		300000,
		10000,
	}

	for i, n := range sizes {
		h.put(numKey(i), strings.Repeat(fmt.Sprintf("v%09d", i), int(n)/10))
	}

	for r := 0; r < 3; r++ {
		h.reopenDB()

		var x uint64
		for i, n := range sizes {
			y := x
			if i > 0 {
				y += 1000
			}
			h.sizeAssert("", numKey(i), x, y)
			x += n
		}

		h.sizeAssert(numKey(3), numKey(5), 110000, 111000)

		h.compactRangeAt(0, "", "")
	}

	h.close()
}

func TestDb_Snapshot(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		h.put("foo", "v1")
		s1 := h.getSnapshot()
		h.put("foo", "v2")
		s2 := h.getSnapshot()
		h.put("foo", "v3")
		s3 := h.getSnapshot()
		h.put("foo", "v4")

		h.getValr(s1, "foo", "v1")
		h.getValr(s2, "foo", "v2")
		h.getValr(s3, "foo", "v3")
		h.getVal("foo", "v4")

		s3.Release()
		h.getValr(s1, "foo", "v1")
		h.getValr(s2, "foo", "v2")
		h.getVal("foo", "v4")

		s1.Release()
		h.getValr(s2, "foo", "v2")
		h.getVal("foo", "v4")

		s2.Release()
		h.getVal("foo", "v4")
	})
}

func TestDb_HiddenValuesAreRemoved(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		s := h.db.s

		h.put("foo", "v1")
		h.compactMem()
		m := kMaxMemCompactLevel
		num := s.version().tLen(m)
		if num != 1 {
			t.Errorf("invalid level-%d len, want=1 got=%d", m, num)
		}

		// Place a table at level last-1 to prevent merging with preceding mutation
		h.put("a", "begin")
		h.put("z", "end")
		h.compactMem()
		v := s.version()
		if v.tLen(m) != 1 {
			t.Errorf("invalid level-%d len, want=1 got=%d", m, v.tLen(m))
		}
		if v.tLen(m-1) != 1 {
			t.Errorf("invalid level-%d len, want=1 got=%d", m-1, v.tLen(m-1))
		}

		h.delete("foo")
		h.put("foo", "v2")
		h.allEntriesFor("foo", "[ v2, DEL, v1 ]")
		h.compactMem()
		h.allEntriesFor("foo", "[ v2, DEL, v1 ]")
		h.compactRangeAt(m-2, "", "z")
		// DEL eliminated, but v1 remains because we aren't compacting that level
		// (DEL can be eliminated because v2 hides v1).
		h.allEntriesFor("foo", "[ v2, v1 ]")
		h.compactRangeAt(m-1, "", "")
		// Merging last-1 w/ last, so we are the base level for "foo", so
		// DEL is removed.  (as is v1).
		h.allEntriesFor("foo", "[ v2 ]")
	})
}

func TestDb_DeletionMarkers2(t *testing.T) {
	h := newDbHarness(t)
	s := h.db.s

	h.put("foo", "v1")
	h.compactMem()
	m := kMaxMemCompactLevel
	num := s.version().tLen(m)
	if num != 1 {
		t.Errorf("invalid level-%d len, want=1 got=%d", m, num)
	}

	// Place a table at level last-1 to prevent merging with preceding mutation
	h.put("a", "begin")
	h.put("z", "end")
	h.compactMem()
	v := s.version()
	if v.tLen(m) != 1 {
		t.Errorf("invalid level-%d len, want=1 got=%d", m, v.tLen(m))
	}
	if v.tLen(m-1) != 1 {
		t.Errorf("invalid level-%d len, want=1 got=%d", m-1, v.tLen(m-1))
	}

	h.delete("foo")
	h.allEntriesFor("foo", "[ DEL, v1 ]")
	h.compactMem() // Moves to level last-2
	h.allEntriesFor("foo", "[ DEL, v1 ]")
	h.compactRangeAt(m-2, "", "")
	// DEL kept: "last" file overlaps
	h.allEntriesFor("foo", "[ DEL, v1 ]")
	h.compactRangeAt(m-1, "", "")
	// Merging last-1 w/ last, so we are the base level for "foo", so
	// DEL is removed.  (as is v1).
	h.allEntriesFor("foo", "[ ]")

	h.close()
}

func TestDb_OverlapInLevel0(t *testing.T) {
	runAllOpts(t, func(h *dbHarness) {
		if kMaxMemCompactLevel != 2 {
			t.Fatal("fix test to reflect the config")
		}

		// Fill levels 1 and 2 to disable the pushing of new memtables to levels > 0.
		h.put("100", "v100")
		h.put("999", "v999")
		h.compactMem()
		h.delete("100")
		h.delete("999")
		h.compactMem()
		h.tablesPerLevel("0,1,1")

		// Make files spanning the following ranges in level-0:
		//  files[0]  200 .. 900
		//  files[1]  300 .. 500
		// Note that files are sorted by min key.
		h.put("300", "v300")
		h.put("500", "v500")
		h.compactMem()
		h.put("200", "v200")
		h.put("600", "v600")
		h.put("900", "v900")
		h.compactMem()
		h.tablesPerLevel("2,1,1")

		// Compact away the placeholder files we created initially
		h.compactRangeAt(1, "", "")
		h.compactRangeAt(2, "", "")
		h.tablesPerLevel("2")

		// Do a memtable compaction.  Before bug-fix, the compaction would
		// not detect the overlap with level-0 files and would incorrectly place
		// the deletion in a deeper level.
		h.delete("600")
		h.compactMem()
		h.tablesPerLevel("3")
		h.get("600", false)
	})
}

func TestDb_L0_CompactionBug_Issue44_a(t *testing.T) {
	h := newDbHarness(t)

	h.reopenDB()
	h.put("b", "v")
	h.reopenDB()
	h.delete("b")
	h.delete("a")
	h.reopenDB()
	h.delete("a")
	h.reopenDB()
	h.put("a", "v")
	h.reopenDB()
	h.reopenDB()
	h.getKeyVal("(a->v)")
	h.db.cch <- cWait
	h.getKeyVal("(a->v)")

	h.close()
}

func TestDb_L0_CompactionBug_Issue44_b(t *testing.T) {
	h := newDbHarness(t)

	h.reopenDB()
	h.put("", "")
	h.reopenDB()
	h.delete("e")
	h.put("", "")
	h.reopenDB()
	h.put("c", "cv")
	h.reopenDB()
	h.put("", "")
	h.reopenDB()
	h.put("", "")
	h.db.cch <- cWait
	h.reopenDB()
	h.put("d", "dv")
	h.reopenDB()
	h.put("", "")
	h.reopenDB()
	h.delete("d")
	h.delete("b")
	h.reopenDB()
	h.getKeyVal("(->)(c->cv)")
	h.db.cch <- cWait
	h.getKeyVal("(->)(c->cv)")

	h.close()
}

func TestDb_ManifestWriteError(t *testing.T) {
	for i := 0; i < 2; i++ {
		h := newDbHarness(t)

		h.put("foo", "bar")
		h.getVal("foo", "bar")

		// Mem compaction (will succeed)
		h.compactMem()
		h.getVal("foo", "bar")
		if n := h.db.s.version().tLen(kMaxMemCompactLevel); n != 1 {
			t.Errorf("invalid total tables, want=1 got=%d", n)
		}

		if i == 0 {
			h.stor.SetWriteErr(storage.TypeManifest)
		} else {
			h.stor.SetSyncErr(storage.TypeManifest)
		}

		// Merging compaction (will fail)
		h.compactRangeAtErr(kMaxMemCompactLevel, "", "", true)

		h.db.Close()
		h.stor.SetWriteErr(0)
		h.stor.SetSyncErr(0)

		// Should not lose data
		h.openDB()
		h.getVal("foo", "bar")

		h.close()
	}
}

func assertErr(t *testing.T, err error, wanterr bool) {
	if err != nil {
		if wanterr {
			t.Log("AssertErr: got error (expected): ", err)
		} else {
			t.Error("AssertErr: got error: ", err)
		}
	} else if wanterr {
		t.Error("AssertErr: expect error")
	}
}

func TestDb_ClosedIsClosed(t *testing.T) {
	h := newDbHarness(t)
	db := h.db

	h.put("k", "v")
	h.getVal("k", "v")

	iter := db.NewIterator(h.ro)
	iter.Seek([]byte("k"))
	testKeyVal(t, iter, "k->v")

	snap, err := db.GetSnapshot()
	if err != nil {
		t.Fatal("GetSnapshot: got error: ", err)
	}

	h.getValr(snap, "k", "v")

	iter2 := snap.NewIterator(h.ro)
	iter2.Seek([]byte("k"))
	testKeyVal(t, iter2, "k->v")

	h.put("foo", "v2")
	h.delete("foo")

	// closing DB
	h.closeDB()

	assertErr(t, db.Put([]byte("x"), []byte("y"), h.wo), true)
	_, err = db.Get([]byte("k"), h.ro)
	assertErr(t, err, true)

	if iter.Valid() {
		t.Errorf("iter.Valid should false")
	}
	assertErr(t, iter.Error(), true)
	testKeyVal(t, iter, "->")
	if iter.Seek([]byte("k")) {
		t.Errorf("iter.Seek should false")
	}
	assertErr(t, iter.Error(), true)

	assertErr(t, iter2.Error(), true)

	_, err = snap.Get([]byte("k"), h.ro)
	assertErr(t, err, true)

	_, err = db.GetSnapshot()
	assertErr(t, err, true)

	iter3 := db.NewIterator(h.ro)
	assertErr(t, iter3.Error(), true)

	iter3 = snap.NewIterator(h.ro)
	assertErr(t, iter3.Error(), true)

	assertErr(t, db.Delete([]byte("k"), h.wo), true)

	_, err = db.GetProperty("leveldb.stats")
	assertErr(t, err, true)

	_, err = db.GetApproximateSizes([]Range{{[]byte("a"), []byte("z")}})
	assertErr(t, err, true)

	assertErr(t, db.CompactRange(Range{}), true)

	assertErr(t, db.Close(), true)
}

type numberComparer struct{}

func (numberComparer) num(x []byte) (n int) {
	fmt.Sscan(string(x[1:len(x)-1]), &n)
	return
}

func (numberComparer) Name() string {
	return "test.NumberComparer"
}

func (p numberComparer) Compare(a, b []byte) int {
	return p.num(a) - p.num(b)
}

func (numberComparer) Separator(a, b []byte) []byte {
	return a
}

func (numberComparer) Successor(b []byte) []byte {
	return b
}

func TestDb_CustomComparer(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{
		Comparer:    numberComparer{},
		WriteBuffer: 1000,
	})

	h.put("[10]", "ten")
	h.put("[0x14]", "twenty")
	for i := 0; i < 2; i++ {
		h.getVal("[10]", "ten")
		h.getVal("[0xa]", "ten")
		h.getVal("[20]", "twenty")
		h.getVal("[0x14]", "twenty")
		h.get("[15]", false)
		h.get("[0xf]", false)
		h.compactMem()
		h.compactRange("[0]", "[9999]")
	}

	for n := 0; n < 2; n++ {
		for i := 0; i < 100; i++ {
			v := fmt.Sprintf("[%d]", i*10)
			h.put(v, v)
		}
		h.compactMem()
		h.compactRange("[0]", "[1000000]")
	}

	h.close()
}

func TestDb_ManualCompaction(t *testing.T) {
	h := newDbHarness(t)

	if kMaxMemCompactLevel != 2 {
		t.Fatal("fix test to reflect the config")
	}

	h.putMulti(3, "p", "q")
	h.tablesPerLevel("1,1,1")

	// Compaction range falls before files
	h.compactRange("", "c")
	h.tablesPerLevel("1,1,1")

	// Compaction range falls after files
	h.compactRange("r", "z")
	h.tablesPerLevel("1,1,1")

	// Compaction range overlaps files
	h.compactRange("p1", "p9")
	h.tablesPerLevel("0,0,1")

	// Populate a different range
	h.putMulti(3, "c", "e")
	h.tablesPerLevel("1,1,2")

	// Compact just the new range
	h.compactRange("b", "f")
	h.tablesPerLevel("0,0,2")

	// Compact all
	h.putMulti(1, "a", "z")
	h.tablesPerLevel("0,1,2")
	h.compactRange("", "")
	h.tablesPerLevel("0,0,1")

	h.close()
}

func TestDb_BloomFilter(t *testing.T) {
	h := newDbHarnessWopt(t, &opt.Options{
		BlockCache: cache.EmptyCache{},
		Filter:     filter.NewBloomFilter(10),
	})

	key := func(i int) string {
		return fmt.Sprintf("key%06d", i)
	}

	n := 10000

	// Populate multiple layers
	for i := 0; i < n; i++ {
		h.put(key(i), key(i))
	}
	h.compactMem()
	h.compactRange("a", "z")
	for i := 0; i < n; i += 100 {
		h.put(key(i), key(i))
	}
	h.compactMem()

	// Prevent auto compactions triggered by seeks
	h.stor.DelaySync(storage.TypeTable)

	// Lookup present keys. Should rarely read from small sstable.
	h.stor.SetReadAtCounter(storage.TypeTable)
	for i := 0; i < n; i++ {
		h.getVal(key(i), key(i))
	}
	cnt := int(h.stor.ReadAtCounter())
	t.Logf("lookup of %d present keys yield %d sstable I/O reads", n, cnt)

	if min, max := n, n+2*n/100; cnt < min || cnt > max {
		t.Errorf("num of sstable I/O reads of present keys not in range of %d - %d, got %d", min, max, cnt)
	}

	// Lookup missing keys. Should rarely read from either sstable.
	h.stor.ResetReadAtCounter()
	for i := 0; i < n; i++ {
		h.get(key(i)+".missing", false)
	}
	cnt = int(h.stor.ReadAtCounter())
	t.Logf("lookup of %d missing keys yield %d sstable I/O reads", n, cnt)
	if max := 3 * n / 100; cnt > max {
		t.Errorf("num of sstable I/O reads of missing keys was more than %d, got %d", max, cnt)
	}

	h.stor.ReleaseSync(storage.TypeTable)
	h.close()
}

func TestDb_Concurrent(t *testing.T) {
	const n, secs, maxkey = 4, 2, 1000

	runtime.GOMAXPROCS(n)
	runAllOpts(t, func(h *dbHarness) {
		var wg sync.WaitGroup
		var stop uint32
		var cnt [n]uint32

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				var put, get, found uint
				defer func() {
					t.Logf("goroutine %d stopped after %d ops, put=%d get=%d found=%d missing=%d",
						i, cnt[i], put, get, found, get-found)
					wg.Done()
				}()

				rnd := rand.New(rand.NewSource(int64(1000 + i)))
				for atomic.LoadUint32(&stop) == 0 {
					x := cnt[i]

					k := rnd.Intn(maxkey)
					kstr := fmt.Sprintf("%016d", k)

					if (rnd.Int() % 2) > 0 {
						put++
						h.put(kstr, fmt.Sprintf("%d.%d.%-1000d", k, i, x))
					} else {
						get++
						v, err := h.db.Get([]byte(kstr), h.ro)
						if err == nil {
							found++
							rk, ri, rx := 0, -1, uint32(0)
							fmt.Sscanf(string(v), "%d.%d.%d", &rk, &ri, &rx)
							if rk != k {
								t.Errorf("invalid key want=%d got=%d", k, rk)
							}
							if ri < 0 || ri >= n {
								t.Error("invalid goroutine number: ", ri)
							} else {
								tx := atomic.LoadUint32(&(cnt[ri]))
								if rx > tx {
									t.Errorf("invalid seq number, %d > %d ", rx, tx)
								}
							}
						} else if err != errors.ErrNotFound {
							t.Error("Get: got error: ", err)
							return
						}
					}
					atomic.AddUint32(&cnt[i], 1)
				}
			}(i)
		}

		for i := 0; i < secs; i++ {
			h.oo.SetBlockCache(cache.NewLRUCache(rand.Int() % (opt.DefaultBlockCacheSize * 2)))
			time.Sleep(time.Second)
		}
		atomic.StoreUint32(&stop, 1)
		wg.Wait()
	})

	runtime.GOMAXPROCS(1)
}

func TestDb_ModifyingBuffer(t *testing.T) {
	h := newDbHarness(t)
	defer h.close()

	h.put("foo", "hello")

	modifyBuffer := func() {
		iter := h.db.NewIterator(&opt.ReadOptions{})
		if !iter.First() {
			t.Fatal("iter.First failed: ", iter.Error())
		}
		key, value := iter.Key(), iter.Value()
		copy(key, []byte("bar"))
		copy(value, []byte("olleh"))
	}

	modifyBuffer()
	h.getKeyVal("(foo->hello)")
	h.compactMem()
	if h.totalTables() == 0 {
		t.Fatal("expecting more than zero table")
	}
	modifyBuffer()
	h.getKeyVal("(foo->hello)")
}

func TestDb_CreateReopenDbOnFile(t *testing.T) {
	dbpath := filepath.Join(os.TempDir(), fmt.Sprintf("goleveldbtestCreateReopenDbOnFile-%d", os.Getuid()))
	if err := os.RemoveAll(dbpath); err != nil {
		t.Fatal("cannot remove old db: ", err)
	}
	defer os.RemoveAll(dbpath)

	for i := 0; i < 3; i++ {
		stor, err := storage.OpenFile(dbpath)
		if err != nil {
			t.Fatalf("(%d) cannot open storage: %s", i, err)
		}
		db, err := Open(stor, &opt.Options{Flag: opt.OFCreateIfMissing})
		if err != nil {
			t.Fatalf("(%d) cannot open db: %s", i, err)
		}
		if err := db.Put([]byte("foo"), []byte("bar"), &opt.WriteOptions{}); err != nil {
			t.Fatalf("(%d) cannot write to db: %s", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("(%d) cannot close db: %s", i, err)
		}
		if err := stor.Close(); err != nil {
			t.Fatalf("(%d) cannot close storage: %s", i, err)
		}
	}
}

func TestDb_CreateReopenDbOnFile2(t *testing.T) {
	dbpath := filepath.Join(os.TempDir(), fmt.Sprintf("goleveldbtestCreateReopenDbOnFile2-%d", os.Getuid()))
	if err := os.RemoveAll(dbpath); err != nil {
		t.Fatal("cannot remove old db: ", err)
	}
	defer os.RemoveAll(dbpath)

	for i := 0; i < 3; i++ {
		db, err := OpenFile(dbpath, &opt.Options{Flag: opt.OFCreateIfMissing})
		if err != nil {
			t.Fatalf("(%d) cannot open db: %s", i, err)
		}
		if err := db.Put([]byte("foo"), []byte("bar"), &opt.WriteOptions{}); err != nil {
			t.Fatalf("(%d) cannot write to db: %s", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("(%d) cannot close db: %s", i, err)
		}
	}
}
