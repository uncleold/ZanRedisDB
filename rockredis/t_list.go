package rockredis

import (
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/absolute8511/gorocksdb"
)

// TODO: we can use ring buffer to allow the list pop and push many times
// when the tail reach the end we roll to the start and check if full.
const (
	listHeadSeq int32 = 1
	listTailSeq int32 = 2

	listMinSeq     int32 = 1000
	listMaxSeq     int32 = 1<<31 - 1000
	listInitialSeq int32 = listMinSeq + (listMaxSeq-listMinSeq)/2
)

var errLMetaKey = errors.New("invalid lmeta key")
var errListKey = errors.New("invalid list key")
var errListSeq = errors.New("invalid list sequence, overflow")

func lEncodeMetaKey(key []byte) []byte {
	buf := make([]byte, len(key)+1)
	pos := 0
	buf[pos] = LMetaType
	pos++

	copy(buf[pos:], key)
	return buf
}

func lDecodeMetaKey(ek []byte) ([]byte, error) {
	pos := 0
	if pos+1 > len(ek) || ek[pos] != LMetaType {
		return nil, errLMetaKey
	}

	pos++
	return ek[pos:], nil
}

func lEncodeMinKey() []byte {
	return lEncodeMetaKey(nil)
}

func lEncodeMaxKey() []byte {
	ek := lEncodeMetaKey(nil)
	ek[len(ek)-1] = LMetaType + 1
	return ek
}

func lEncodeListKey(key []byte, seq int32) []byte {
	buf := make([]byte, len(key)+7)

	pos := 0
	buf[pos] = ListType
	pos++

	binary.BigEndian.PutUint16(buf[pos:], uint16(len(key)))
	pos += 2

	copy(buf[pos:], key)
	pos += len(key)

	binary.BigEndian.PutUint32(buf[pos:], uint32(seq))

	return buf
}

func lDecodeListKey(ek []byte) (key []byte, seq int32, err error) {
	pos := 0
	if pos+1 > len(ek) || ek[pos] != ListType {
		err = errListKey
		return
	}

	pos++

	if pos+2 > len(ek) {
		err = errListKey
		return
	}

	keyLen := int(binary.BigEndian.Uint16(ek[pos:]))
	pos += 2
	if keyLen+pos+4 != len(ek) {
		err = errListKey
		return
	}

	key = ek[pos : pos+keyLen]
	seq = int32(binary.BigEndian.Uint32(ek[pos+keyLen:]))
	return
}

func (db *RockDB) lpush(key []byte, whereSeq int32, args ...[]byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}

	var headSeq int32
	var tailSeq int32
	var size int32
	var err error

	wb := gorocksdb.NewWriteBatch()
	defer wb.Destroy()
	metaKey := lEncodeMetaKey(key)
	headSeq, tailSeq, size, err = db.lGetMeta(metaKey)
	if err != nil {
		return 0, err
	}

	var pushCnt int = len(args)
	if pushCnt == 0 {
		return int64(size), nil
	}

	var seq int32 = headSeq
	var delta int32 = -1
	if whereSeq == listTailSeq {
		seq = tailSeq
		delta = 1
	}

	//	append elements
	if size > 0 {
		seq += delta
	}

	checkSeq := seq + int32(pushCnt-1)*delta
	if checkSeq <= listMinSeq || checkSeq >= listMaxSeq {
		return 0, errListSeq
	}
	for i := 0; i < pushCnt; i++ {
		ek := lEncodeListKey(key, seq+int32(i)*delta)
		wb.Put(ek, args[i])
	}
	seq += int32(pushCnt-1) * delta
	//	set meta info
	if whereSeq == listHeadSeq {
		headSeq = seq
	} else {
		tailSeq = seq
	}

	db.lSetMeta(metaKey, headSeq, tailSeq, wb)
	err = db.eng.Write(db.defaultWriteOpts, wb)
	return int64(size) + int64(pushCnt), err
}

func (db *RockDB) lpop(key []byte, whereSeq int32) ([]byte, error) {
	if err := checkKeySize(key); err != nil {
		return nil, err
	}

	wb := gorocksdb.NewWriteBatch()
	defer wb.Destroy()

	var headSeq int32
	var tailSeq int32
	var size int32
	var err error

	metaKey := lEncodeMetaKey(key)
	headSeq, tailSeq, size, err = db.lGetMeta(metaKey)
	if err != nil {
		return nil, err
	} else if size == 0 {
		return nil, nil
	}

	var value []byte

	var seq int32 = headSeq
	if whereSeq == listTailSeq {
		seq = tailSeq
	}

	itemKey := lEncodeListKey(key, seq)
	value, err = db.eng.GetBytes(db.defaultReadOpts, itemKey)
	if err != nil {
		return nil, err
	}

	if whereSeq == listHeadSeq {
		headSeq += 1
	} else {
		tailSeq -= 1
	}

	wb.Delete(itemKey)
	size, err = db.lSetMeta(metaKey, headSeq, tailSeq, wb)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		// list is empty after delete
	}
	err = db.eng.Write(db.defaultWriteOpts, wb)
	return value, err
}

func (db *RockDB) ltrim2(key []byte, startP, stopP int64) error {
	if err := checkKeySize(key); err != nil {
		return err
	}

	wb := gorocksdb.NewWriteBatch()
	defer wb.Destroy()

	var headSeq int32
	var llen int32
	var err error
	start := int32(startP)
	stop := int32(stopP)

	ek := lEncodeMetaKey(key)
	if headSeq, _, llen, err = db.lGetMeta(ek); err != nil {
		return err
	} else {
		if start < 0 {
			start = llen + start
		}
		if stop < 0 {
			stop = llen + stop
		}
		if start >= llen || start > stop {
			//db.lDelete(key, wb)
			return errors.New("trim invalid")
		}

		if start < 0 {
			start = 0
		}
		if stop >= llen {
			stop = llen - 1
		}
	}

	if start > 0 {
		for i := int32(0); i < start; i++ {
			wb.Delete(lEncodeListKey(key, headSeq+i))
		}
	}
	if stop < int32(llen-1) {
		for i := int32(stop + 1); i < llen; i++ {
			wb.Delete(lEncodeListKey(key, headSeq+i))
		}
	}

	db.lSetMeta(ek, headSeq+start, headSeq+stop, wb)

	return db.eng.Write(db.defaultWriteOpts, wb)
}

func (db *RockDB) ltrim(key []byte, trimSize, whereSeq int32) (int32, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}

	if trimSize == 0 {
		return 0, nil
	}

	wb := gorocksdb.NewWriteBatch()
	defer wb.Destroy()

	var headSeq int32
	var tailSeq int32
	var size int32
	var err error

	metaKey := lEncodeMetaKey(key)
	headSeq, tailSeq, size, err = db.lGetMeta(metaKey)
	if err != nil {
		return 0, err
	} else if size == 0 {
		return 0, nil
	}

	var (
		trimStartSeq int32
		trimEndSeq   int32
	)

	if whereSeq == listHeadSeq {
		trimStartSeq = headSeq
		trimEndSeq = MinInt32(trimStartSeq+trimSize-1, tailSeq)
		headSeq = trimEndSeq + 1
	} else {
		trimEndSeq = tailSeq
		trimStartSeq = MaxInt32(trimEndSeq-trimSize+1, headSeq)
		tailSeq = trimStartSeq - 1
	}

	for trimSeq := trimStartSeq; trimSeq <= trimEndSeq; trimSeq++ {
		itemKey := lEncodeListKey(key, trimSeq)
		wb.Delete(itemKey)
	}

	size, err = db.lSetMeta(metaKey, headSeq, tailSeq, wb)
	if err != nil {
		return 0, err
	}
	if size == 0 {
		// list is empty after trim
	}

	err = db.eng.Write(db.defaultWriteOpts, wb)
	return trimEndSeq - trimStartSeq + 1, err
}

//	ps : here just focus on deleting the list data,
//		 any other likes expire is ignore.
func (db *RockDB) lDelete(key []byte, wb *gorocksdb.WriteBatch) int64 {
	mk := lEncodeMetaKey(key)

	var headSeq int32
	var tailSeq int32
	var size int32
	var err error

	headSeq, tailSeq, size, err = db.lGetMeta(mk)
	if err != nil {
		return 0
	}

	var num int64 = 0
	startKey := lEncodeListKey(key, headSeq)
	stopKey := lEncodeListKey(key, tailSeq)
	if size > RANGE_DELETE_NUM {
		var r gorocksdb.Range
		r.Start = startKey
		r.Limit = stopKey
		db.eng.DeleteFilesInRange(r)
		db.eng.CompactRange(r)
	}

	rit := db.eng.NewIterator(db.defaultReadOpts)
	defer rit.Close()
	rit.Seek(startKey)
	for ; rit.Valid(); rit.Next() {
		rawk := rit.Key().Data()
		if bytes.Compare(rawk, stopKey) > 0 {
			break
		}
		wb.Delete(rawk)
		num++
	}

	wb.Delete(mk)
	return num
}

func (db *RockDB) lGetMeta(ek []byte) (headSeq int32, tailSeq int32, size int32, err error) {
	var v []byte
	v, err = db.eng.GetBytes(db.defaultReadOpts, ek)
	if err != nil {
		return
	} else if v == nil {
		headSeq = listInitialSeq
		tailSeq = listInitialSeq
		size = 0
		return
	} else {
		headSeq = int32(binary.BigEndian.Uint32(v[0:4]))
		tailSeq = int32(binary.BigEndian.Uint32(v[4:8]))
		size = tailSeq - headSeq + 1
	}
	return
}

func (db *RockDB) lSetMeta(ek []byte, headSeq int32, tailSeq int32, wb *gorocksdb.WriteBatch) (int32, error) {
	var size int32 = tailSeq - headSeq + 1
	if size < 0 {
		//	todo : log error + panic
		//log.Fatalf("invalid meta sequence range [%d, %d]", headSeq, tailSeq)
		return 0, errListSeq
	} else if size == 0 {
		wb.Delete(ek)
	} else {
		buf := make([]byte, 8)
		binary.BigEndian.PutUint32(buf[0:4], uint32(headSeq))
		binary.BigEndian.PutUint32(buf[4:8], uint32(tailSeq))
		wb.Put(ek, buf)
	}
	return size, nil
}

func (db *RockDB) LIndex(key []byte, index int32) ([]byte, error) {
	if err := checkKeySize(key); err != nil {
		return nil, err
	}

	var seq int32
	var headSeq int32
	var tailSeq int32
	var err error

	metaKey := lEncodeMetaKey(key)

	headSeq, tailSeq, _, err = db.lGetMeta(metaKey)
	if err != nil {
		return nil, err
	}

	if index >= 0 {
		seq = headSeq + index
	} else {
		seq = tailSeq + index + 1
	}

	sk := lEncodeListKey(key, seq)
	return db.eng.GetBytes(db.defaultReadOpts, sk)
}

func (db *RockDB) LLen(key []byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}

	ek := lEncodeMetaKey(key)
	_, _, size, err := db.lGetMeta(ek)
	return int64(size), err
}

func (db *RockDB) LPop(key []byte) ([]byte, error) {
	return db.lpop(key, listHeadSeq)
}

func (db *RockDB) LTrim(key []byte, start, stop int64) error {
	return db.ltrim2(key, start, stop)
}

func (db *RockDB) LTrimFront(key []byte, trimSize int32) (int32, error) {
	return db.ltrim(key, trimSize, listHeadSeq)
}

func (db *RockDB) LTrimBack(key []byte, trimSize int32) (int32, error) {
	return db.ltrim(key, trimSize, listTailSeq)
}

func (db *RockDB) LPush(key []byte, args ...[]byte) (int64, error) {
	return db.lpush(key, listHeadSeq, args...)
}
func (db *RockDB) LSet(key []byte, index int32, value []byte) error {
	if err := checkKeySize(key); err != nil {
		return err
	}

	var seq int32
	var headSeq int32
	var tailSeq int32
	//var size int32
	var err error
	metaKey := lEncodeMetaKey(key)

	headSeq, tailSeq, _, err = db.lGetMeta(metaKey)
	if err != nil {
		return err
	}

	if index >= 0 {
		seq = headSeq + index
	} else {
		seq = tailSeq + index + 1
	}
	if seq < headSeq || seq > tailSeq {
		return errListIndex
	}
	sk := lEncodeListKey(key, seq)
	err = db.eng.Put(db.defaultWriteOpts, sk, value)
	return err
}

func (db *RockDB) LRange(key []byte, start int32, stop int32) ([][]byte, error) {
	if err := checkKeySize(key); err != nil {
		return nil, err
	}

	var headSeq int32
	var llen int32
	var err error

	metaKey := lEncodeMetaKey(key)

	if headSeq, _, llen, err = db.lGetMeta(metaKey); err != nil {
		return nil, err
	}

	if start < 0 {
		start = llen + start
	}
	if stop < 0 {
		stop = llen + stop
	}
	if start < 0 {
		start = 0
	}

	if start > stop || start >= llen {
		return [][]byte{}, nil
	}

	if stop >= llen {
		stop = llen - 1
	}

	limit := (stop - start) + 1
	headSeq += start

	v := make([][]byte, 0, limit)

	startKey := lEncodeListKey(key, headSeq)
	rit := db.eng.NewIterator(db.defaultReadOpts)
	defer rit.Close()
	rit.Seek(startKey)
	for ; rit.Valid(); rit.Next() {
		v = append(v, rit.Value().Data())
		if len(v) >= int(limit) {
			break
		}
	}
	return v, nil
}

func (db *RockDB) RPop(key []byte) ([]byte, error) {
	return db.lpop(key, listTailSeq)
}

func (db *RockDB) RPush(key []byte, args ...[]byte) (int64, error) {
	return db.lpush(key, listTailSeq, args...)
}

func (db *RockDB) LClear(key []byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}
	wb := gorocksdb.NewWriteBatch()
	defer wb.Destroy()
	num := db.lDelete(key, wb)
	err := db.eng.Write(db.defaultWriteOpts, wb)
	if err != nil {
		// TODO: log here , the list maybe corrupt
	}

	return num, err
}

func (db *RockDB) LMclear(keys ...[]byte) (int64, error) {
	wb := gorocksdb.NewWriteBatch()
	defer wb.Destroy()
	for _, key := range keys {
		if err := checkKeySize(key); err != nil {
			return 0, err
		}
		db.lDelete(key, wb)
	}
	err := db.eng.Write(db.defaultWriteOpts, wb)
	if err != nil {
		// TODO: log here , the list maybe corrupt
	}

	return int64(len(keys)), err
}

func (db *RockDB) LKeyExists(key []byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}
	sk := lEncodeMetaKey(key)
	v, err := db.eng.GetBytes(db.defaultReadOpts, sk)
	if v != nil && err == nil {
		return 1, nil
	}
	return 0, err
}
