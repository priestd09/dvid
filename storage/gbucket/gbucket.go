// +build gbucket

package gbucket

/*
TODO:

* (maybe) Allow creation of new bucket (using http API)
* Encode keys with random test prefix for testing
* Improve error handling (more expressive print statements)
* Refactor to call batcher for multiple DB requests.  Consider multipart http requests.
Explore tradeoff between smaller parallel requests and single big requests.
* Restrict the number of parallel requests.
* Refactor to call batcher for any calls that require multiple requests
* DeleteAll should page deletion to avoid memory issues for very large deletes

Note:

* Range calls require a list query to find potentially matching objects.  After this
call, specific objects can be fetched.
* Lists are eventually consistent and objects are strongly consistent after object post/change.
It is possible to post an object and not see the object when searching the list.
* The Batcher implementation does not wrap operations into an atomic transaction.


*/

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/storage"
	"github.com/janelia-flyem/go/semver"
	"io/ioutil"
	"sort"
	"sync"

	"golang.org/x/net/context"
	api "google.golang.org/cloud/storage"
)

func init() {
	ver, err := semver.Make("0.1.0")
	if err != nil {
		dvid.Errorf("Unable to make semver in gbucket: %v\n", err)
	}
	e := Engine{"gbucket", "Google's Storage Bucket", ver}
	storage.RegisterEngine(e)
}

const (
	// limit the number of parallel requests
	MAXCONNECTIONS = 100
	INITKEY        = "initialized"
)

// --- Engine Implementation ------

type Engine struct {
	name   string
	desc   string
	semver semver.Version
}

func (e Engine) GetName() string {
	return e.name
}

func (e Engine) GetDescription() string {
	return e.desc
}

func (e Engine) GetSemVer() semver.Version {
	return e.semver
}

func (e Engine) String() string {
	return fmt.Sprintf("%s [%s]", e.name, e.semver)
}

// NewMetaDataStore returns a storage bucket suitable for MetaData storage.
// The passed Config must contain:
// "bucket": name of bucket
func (e Engine) NewMetaDataStore(config dvid.EngineConfig) (storage.MetaDataStorer, bool, error) {
	return e.newGBucket(config)
}

// NewMetaDataStore returns a storage bucket suitable for MetaData storage.
// The passed Config must contain:
// "bucket": name of bucket
func (e Engine) NewMutableStore(config dvid.EngineConfig) (storage.MutableStorer, bool, error) {
	return e.newGBucket(config)
}

// NewMetaDataStore returns a storage bucket suitable for MetaData storage.
// The passed Config must contain:
// "bucket": name of bucket
func (e Engine) NewImmutableStore(config dvid.EngineConfig) (storage.ImmutableStorer, bool, error) {
	return e.newGBucket(config)
}

// parseConfig initializes GBucket from config
func parseConfig(config dvid.EngineConfig) (*GBucket, error) {

	gb := &GBucket{
		bname: config.Bucket,
		ctx:   context.Background(),
	}

	return gb, nil
}

// newGBucket sets up admin client (bucket must already exist)
func (e *Engine) newGBucket(config dvid.EngineConfig) (*GBucket, bool, error) {
	gb, err := parseConfig(config)
	if err != nil {
		return nil, false, fmt.Errorf("Error in newGBucket() %s\n", err)
	}

	// NewClient uses Application Default Credentials to authenticate.
	gb.client, err = api.NewClient(gb.ctx)
	if err != nil {
		return nil, false, err
	}

	// bucket must already exist -- check existence
	gb.bucket = gb.client.Bucket(gb.bname)
	_, err = gb.bucket.Attrs(gb.ctx)
	if err != nil {
		return nil, false, err
	}

	var created bool
	created = false
	val, err := gb.getV(storage.Key(INITKEY))
	// check if value exists
	if val == nil {
		created = true
		err = gb.putV(storage.Key(INITKEY), make([]byte, 1))
		if err != nil {
			return nil, false, err
		}
	}

	return gb, created, nil
}

// cannot Delete bucket from API
func (e Engine) Delete(config dvid.EngineConfig) error {
	return nil
}

type GBucket struct {
	bname  string
	bucket *api.BucketHandle
	ctx    context.Context
	client *api.Client
}

func (db *GBucket) String() string {
	return "Google's storage bucket"
}

// ---- OrderedKeyValueGetter interface ------

// get retrieves a value from a given key or an error if nothing exists
func (db *GBucket) getV(k storage.Key) ([]byte, error) {

	// gets handle (no network op)
	obj_handle := db.bucket.Object(base64.URLEncoding.EncodeToString(k))

	// returns error if it doesn't exist
	obj, err := obj_handle.NewReader(db.ctx)

	// return nil if not found
	if err == api.ErrObjectNotExist {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value, err := ioutil.ReadAll(obj)
	return value, err
}

// put value from a given key or an error if nothing exists
func (db *GBucket) deleteV(k storage.Key) error {
	// gets handle (no network op)
	obj_handle := db.bucket.Object(base64.URLEncoding.EncodeToString(k))

	return obj_handle.Delete(db.ctx)
}

// put value from a given key or an error if nothing exists
func (db *GBucket) putV(k storage.Key, value []byte) (err error) {
	// gets handle (no network op)
	err = nil
	obj_handle := db.bucket.Object(base64.URLEncoding.EncodeToString(k))

	//debug.PrintStack()
	// returns error if it doesn't exist
	obj := obj_handle.NewWriter(db.ctx)

	// close will flush buffer
	defer func() {
		err2 := obj.Close()
		if err == nil {
			err = err2
		}
	}()

	// write data to buffer
	_, err = obj.Write(value)
	if err != nil {
		return err
	}

	return err
}

// getSingleVersionedKey returns value for latest version
func (db *GBucket) getSingleVersionedKey(vctx storage.VersionedCtx, k []byte) ([]byte, error) {
	// grab all keys within versioned range
	keys, err := db.getKeysInRange(vctx.(storage.Context), k, k)
	if err != nil {
		return nil, err
	}
	if keys == nil || len(keys) == 0 {
		return nil, err
	}
	if len(keys) > 1 {
		return nil, fmt.Errorf("More than one key found given prefix")
	}

	// retrieve actual data
	return db.getV(keys[0])
}

// getKeysInRangeRaw returns all keys in a range (including multiple keys and tombstones)
func (db *GBucket) getKeysInRangeRaw(minKey, maxKey storage.Key) ([]storage.Key, error) {
	keys := make(KeyArray, 0)
	// extract common prefix
	prefix := grabPrefix(minKey, maxKey)

	// iterate through query (default 1000 items at a time)
	extractedlist := false
	query := &api.Query{Prefix: base64.URLEncoding.EncodeToString(prefix)}
	for !extractedlist {
		// query objects
		object_list, _ := db.bucket.List(db.ctx, query)

		// filter keys that fall into range
		for _, object_attr := range object_list.Results {
			decstr, err := base64.URLEncoding.DecodeString(object_attr.Name)
			if err != nil {
				return nil, err
			}
			if bytes.Compare(decstr, minKey) >= 0 && bytes.Compare(decstr, maxKey) <= 0 {
				keys = append(keys, decstr)
			}
		}

		// make another query if there are lot of keys
		if object_list.Next == nil {
			extractedlist = true
		} else {
			query = object_list.Next
		}
	}

	// sort keys
	sort.Sort(keys)

	return []storage.Key(keys), nil
}

// getKeysInRange returns all the latest keys in a range (versioned or unversioned)
func (db *GBucket) getKeysInRange(ctx storage.Context, TkBeg, TkEnd storage.TKey) ([]storage.Key, error) {
	var minKey storage.Key
	var maxKey storage.Key
	var err error

	// extract min and max key
	if !ctx.Versioned() {
		minKey = ctx.ConstructKey(TkBeg)
		maxKey = ctx.ConstructKey(TkEnd)
	} else {
		minKey, err = ctx.(storage.VersionedCtx).MinVersionKey(TkBeg)
		if err != nil {
			return nil, err
		}
		maxKey, err = ctx.(storage.VersionedCtx).MaxVersionKey(TkEnd)
		if err != nil {
			return nil, err
		}
	}

	keys, _ := db.getKeysInRangeRaw(minKey, maxKey)

	// grab latest version if versioned
	if ctx.Versioned() {
		vctx, ok := ctx.(storage.VersionedCtx)
		if !ok {
			return nil, fmt.Errorf("Bad Get(): context is versioned but doesn't fulfill interface: %v", ctx)
		}

		vkeys := make([]storage.Key, 0)
		versionkeymap := make(map[string][]storage.Key)
		for _, key := range keys {
			tk, _ := ctx.TKeyFromKey(key)
			versionkeymap[string(tk)] = append(versionkeymap[string(tk)], key)
		}

		for _, keylist := range versionkeymap {
			tkvs := []*storage.KeyValue{}
			for _, key := range keylist {
				tkvs = append(tkvs, &storage.KeyValue{key, []byte{}})
			}

			// extract relevant version
			kv, _ := vctx.VersionedKeyValue(tkvs)

			if kv != nil {
				vkeys = append(vkeys, kv.K)
			}
		}
		keys = vkeys

		sort.Sort(KeyArray(keys))
	}

	return keys, nil
}

// Get returns a value given a key.
func (db *GBucket) Get(ctx storage.Context, tk storage.TKey) ([]byte, error) {
	if db == nil {
		return nil, fmt.Errorf("Can't call Get() on nil GBucket")
	}
	if ctx == nil {
		return nil, fmt.Errorf("Received nil context in Get()")
	}
	if ctx.Versioned() {
		vctx, ok := ctx.(storage.VersionedCtx)
		if !ok {
			return nil, fmt.Errorf("Bad Get(): context is versioned but doesn't fulfill interface: %v", ctx)
		}

		// Get all versions of this key and return the most recent
		v, err := db.getSingleVersionedKey(vctx, tk)
		if err != nil {
			return nil, err
		}

		storage.StoreValueBytesRead <- len(v)
		return v, err
	} else {
		key := ctx.ConstructKey(tk)
		v, err := db.getV(key)
		storage.StoreValueBytesRead <- len(v)
		return v, err
	}

}

// KeysInRange returns a range of type-specific key components spanning (TkBeg, TkEnd).
func (db *GBucket) KeysInRange(ctx storage.Context, TkBeg, TkEnd storage.TKey) ([]storage.TKey, error) {
	if db == nil {
		return nil, fmt.Errorf("Can't call KeysInRange() on nil Google bucket")
	}
	if ctx == nil {
		return nil, fmt.Errorf("Received nil context in KeysInRange()")
	}

	// grab keys
	keys, _ := db.getKeysInRange(ctx, TkBeg, TkEnd)
	tKeys := make([]storage.TKey, 0)

	// grab only object names within range
	for _, key := range keys {
		tk, err := ctx.TKeyFromKey(key)
		if err != nil {
			return nil, err
		}
		tKeys = append(tKeys, tk)
	}

	return tKeys, nil
}

// SendKeysInRange sends a range of full keys down a key channel.
func (db *GBucket) SendKeysInRange(ctx storage.Context, TkBeg, TkEnd storage.TKey, ch storage.KeyChan) error {
	if db == nil {
		return fmt.Errorf("Can't call SendKeysInRange() on nil Google Bucket")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in SendKeysInRange()")
	}

	// grab keys
	keys, _ := db.getKeysInRange(ctx, TkBeg, TkEnd)

	// grab only object names within range
	for _, key := range keys {
		ch <- key
	}
	ch <- nil
	return nil
}

// GetRange returns a range of values spanning (TkBeg, kEnd) keys.
func (db *GBucket) GetRange(ctx storage.Context, TkBeg, TkEnd storage.TKey) ([]*storage.TKeyValue, error) {
	if db == nil {
		return nil, fmt.Errorf("Can't call GetRange() on nil GBucket")
	}
	if ctx == nil {
		return nil, fmt.Errorf("Received nil context in GetRange()")
	}

	values := make([]*storage.TKeyValue, 0)

	// grab keys
	keys, _ := db.getKeysInRange(ctx, TkBeg, TkEnd)

	// process keys in parallel
	kvmap := make(map[string][]byte)
	for _, key := range keys {
		kvmap[string(key)] = nil
	}

	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func(lkey storage.Key) {
			defer wg.Done()
			value, err := db.getV(lkey)
			if value == nil || err != nil {
				kvmap[string(lkey)] = nil
			} else {
				kvmap[string(lkey)] = value
			}

		}(key)

	}
	wg.Wait()

	var err error
	err = nil
	// return keyvalues
	for key, val := range kvmap {
		tk, err := ctx.TKeyFromKey(storage.Key(key))
		if err != nil {
			return nil, err
		}

		if val == nil {
			return nil, fmt.Errorf("Could not retrieve value")
		}

		tkv := storage.TKeyValue{tk, val}
		values = append(values, &tkv)
	}

	return values, err
}

// ProcessRange sends a range of type key-value pairs to type-specific chunk handlers,
// allowing chunk processing to be concurrent with key-value sequential reads.
// Since the chunks are typically sent during sequential read iteration, the
// receiving function can be organized as a pool of chunk handling goroutines.
// See datatype/imageblk.ProcessChunk() for an example.
func (db *GBucket) ProcessRange(ctx storage.Context, TkBeg, TkEnd storage.TKey, op *storage.ChunkOp, f storage.ChunkFunc) error {
	// use buffer interface
	buffer := db.NewBuffer(ctx)

	err := buffer.ProcessRange(ctx, TkBeg, TkEnd, op, f)

	if err != nil {
		return err
	}

	// commit changes
	err = buffer.Flush()

	return err
}

// RawRangeQuery sends a range of full keys.  This is to be used for low-level data
// retrieval like DVID-to-DVID communication and should not be used by data type
// implementations if possible because each version's key-value pairs are sent
// without filtering by the current version and its ancestor graph.
func (db *GBucket) RawRangeQuery(kStart, kEnd storage.Key, keysOnly bool, out chan *storage.KeyValue) error {
	if db == nil {
		return fmt.Errorf("Can't call RawRangeQuery() on nil Google bucket")
	}

	// grab keys
	keys, _ := db.getKeysInRangeRaw(kStart, kEnd)

	// process keys in parallel
	kvmap := make(map[string][]byte)
	for _, key := range keys {
		kvmap[string(key)] = nil
	}
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func(lkey storage.Key) {
			defer wg.Done()
			value, err := db.getV(lkey)
			if value == nil || err != nil {
				kvmap[string(lkey)] = nil
			} else {
				kvmap[string(lkey)] = value
			}

		}(key)

	}
	wg.Wait()

	// return keyvalues
	for key, val := range kvmap {
		if val == nil {
			return fmt.Errorf("Could not retrieve value")
		}
		kv := storage.KeyValue{storage.Key(key), val}
		out <- &kv
	}

	return nil
}

// Put writes a value with given key in a possibly versioned context.
func (db *GBucket) Put(ctx storage.Context, tkey storage.TKey, value []byte) error {
	// use buffer interface
	buffer := db.NewBuffer(ctx)

	err := buffer.Put(ctx, tkey, value)

	if err != nil {
		return err
	}

	// commit changes
	err = buffer.Flush()

	return err
}

// RawPut is a low-level function that puts a key-value pair using full keys.
// This can be used in conjunction with RawRangeQuery.
func (db *GBucket) RawPut(k storage.Key, v []byte) error {
	// make dummy context for buffer interface
	ctx := storage.NewMetadataContext()

	// use buffer interface
	buffer := db.NewBuffer(ctx)

	err := buffer.RawPut(k, v)

	if err != nil {
		return err
	}

	// commit changes
	err = buffer.Flush()

	return err
}

// Delete deletes a key-value pair so that subsequent Get on the key returns nil.
func (db *GBucket) Delete(ctx storage.Context, tkey storage.TKey) error {
	// use buffer interface
	buffer := db.NewBuffer(ctx)

	err := buffer.Delete(ctx, tkey)

	if err != nil {
		return err
	}

	// commit changes
	err = buffer.Flush()

	return err
}

// RawDelete is a low-level function.  It deletes a key-value pair using full keys
// without any context.  This can be used in conjunction with RawRangeQuery.
func (db *GBucket) RawDelete(fullKey storage.Key) error {
	// make dummy context for buffer interface
	ctx := storage.NewMetadataContext()

	// use buffer interface
	buffer := db.NewBuffer(ctx)

	err := buffer.RawDelete(fullKey)

	if err != nil {
		return err
	}

	// commit changes
	err = buffer.Flush()

	return err
}

// ---- OrderedKeyValueSetter interface ------

// Put key-value pairs.  This is currently executed with parallel requests.
func (db *GBucket) PutRange(ctx storage.Context, kvs []storage.TKeyValue) error {
	// use buffer interface
	buffer := db.NewBuffer(ctx)

	err := buffer.PutRange(ctx, kvs)

	if err != nil {
		return err
	}

	// commit changes
	err = buffer.Flush()

	return err
}

// DeleteRange removes all key-value pairs with keys in the given range.
func (db *GBucket) DeleteRange(ctx storage.Context, TkBeg, TkEnd storage.TKey) error {
	// use buffer interface
	buffer := db.NewBuffer(ctx)

	err := buffer.DeleteRange(ctx, TkBeg, TkEnd)

	if err != nil {
		return err
	}

	// commit changes
	err = buffer.Flush()

	return err
}

// DeleteAll removes all key-value pairs for the context.  If allVersions is true,
// then all versions of the data instance are deleted.  Will not produce any tombstones.
func (db *GBucket) DeleteAll(ctx storage.Context, allVersions bool) error {
	if db == nil {
		return fmt.Errorf("Can't call DeleteAll() on nil Google bucket")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in DeleteAll()")
	}

	var err error
	err = nil
	if allVersions {
		// do batched RAW delete of latest version
		vctx, versioned := ctx.(storage.VersionedCtx)
		var minKey, maxKey storage.Key
		if versioned {
			minTKey := storage.MinTKey(storage.TKeyMinClass)
			maxTKey := storage.MaxTKey(storage.TKeyMaxClass)
			minKey, err = vctx.MinVersionKey(minTKey)
			if err != nil {
				return err
			}
			maxKey, err = vctx.MaxVersionKey(maxTKey)
			if err != nil {
				return err
			}
		} else {
			minKey, maxKey = ctx.KeyRange()
		}

		// fetch all matching keys for context
		keys, _ := db.getKeysInRangeRaw(minKey, maxKey)

		// wait for all deletes to complete -- batch??
		var wg sync.WaitGroup
		for _, key := range keys {
			wg.Add(1)
			go func(lkey storage.Key) {
				defer wg.Done()
				db.RawDelete(lkey)
			}(key)
		}
		wg.Wait()
	} else {
		vctx, versioned := ctx.(storage.VersionedCtx)
		if !versioned {
			return fmt.Errorf("Can't ask for versioned delete from unversioned context: %s", ctx)
		}
		minTKey := storage.MinTKey(storage.TKeyMinClass)
		maxTKey := storage.MaxTKey(storage.TKeyMaxClass)
		minKey, err := vctx.MinVersionKey(minTKey)
		if err != nil {
			return err
		}
		maxKey, err := vctx.MaxVersionKey(maxTKey)
		if err != nil {
			return err
		}

		// fetch all matching keys for context
		keys, err := db.getKeysInRangeRaw(minKey, maxKey)

		// wait for all deletes to complete -- batch??
		var wg sync.WaitGroup
		for _, key := range keys {
			// filter keys that are not current version
			tkey, _ := ctx.TKeyFromKey(key)
			if string(ctx.ConstructKey(tkey)) == string(key) {
				wg.Add(1)
				go func(lkey storage.Key) {
					defer wg.Done()
					db.RawDelete(lkey)
				}(key)
			}
		}
		wg.Wait()
	}

	return nil
}

func (db *GBucket) Close() {
	if db == nil {
		dvid.Errorf("Can't call Close() on nil Google Bucket")
		return
	}

	if db.client != nil {
		db.client.Close()
	}
}

// --- Batcher interface ----

type goBatch struct {
	db  storage.RequestBuffer
	ctx storage.Context
}

// NewBatch returns an implementation that allows batch writes
func (db *GBucket) NewBatch(ctx storage.Context) storage.Batch {
	if ctx == nil {
		dvid.Criticalf("Received nil context in NewBatch()")
		return nil
	}
	return &goBatch{db.NewBuffer(ctx), ctx}
}

// --- Batch interface ---

func (batch *goBatch) Delete(tkey storage.TKey) {

	batch.db.Delete(batch.ctx, tkey)
}

func (batch *goBatch) Put(tkey storage.TKey, value []byte) {
	if batch == nil || batch.ctx == nil {
		dvid.Criticalf("Received nil batch or nil batch context in batch.Put()\n")
		return
	}

	batch.db.Put(batch.ctx, tkey, value)
}

// Commit flushes the buffer
func (batch *goBatch) Commit() error {
	return batch.db.Flush()
}

// --- Buffer interface ----

// opType defines different DB operation types
type opType int

const (
	delOp opType = iota
	delOpIgnoreExists
	delRangeOp
	putOp
	putOpCallback
	getOp
)

// dbOp is element for buffered db operations
type dbOp struct {
	op        opType
	key       storage.Key
	value     []byte
	tkBeg     storage.TKey
	tkEnd     storage.TKey
	chunkop   *storage.ChunkOp
	chunkfunc storage.ChunkFunc
	callback  func()
}

// goBuffer allow operations to be submitted in parallel
type goBuffer struct {
	db  *GBucket
	ctx storage.Context
	ops []dbOp
}

// NewBatch returns an implementation that allows batch writes
func (db *GBucket) NewBuffer(ctx storage.Context) storage.RequestBuffer {
	if ctx == nil {
		dvid.Criticalf("Received nil context in NewBatch()")
		return nil
	}
	return &goBuffer{db, ctx, make([]dbOp, 0)}
}

// --- implement RequestBuffer interface ---

// ProcessRange sends a range of type key-value pairs to type-specific chunk handlers,
// allowing chunk processing to be concurrent with key-value sequential reads.
// Since the chunks are typically sent during sequential read iteration, the
// receiving function can be organized as a pool of chunk handling goroutines.
// See datatype/imageblk.ProcessChunk() for an example.
func (db *goBuffer) ProcessRange(ctx storage.Context, TkBeg, TkEnd storage.TKey, op *storage.ChunkOp, f storage.ChunkFunc) error {
	if db == nil {
		return fmt.Errorf("Can't call GetRange() on nil GBucket")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in GetRange()")
	}

	db.ops = append(db.ops, dbOp{getOp, nil, nil, TkBeg, TkEnd, op, f, nil})

	return nil
}

// Put writes a value with given key in a possibly versioned context.
func (db *goBuffer) Put(ctx storage.Context, tkey storage.TKey, value []byte) error {
	if db == nil {
		return fmt.Errorf("Can't call Put() on nil Google Bucket")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in Put()")
	}

	var err error
	err = nil
	key := ctx.ConstructKey(tkey)
	if !ctx.Versioned() {
		db.ops = append(db.ops, dbOp{op: putOp, key: key, value: value})
	} else {
		vctx, ok := ctx.(storage.VersionedCtx)
		if !ok {
			return fmt.Errorf("Non-versioned context that says it's versioned received in Put(): %v", ctx)
		}
		tombstoneKey := vctx.TombstoneKey(tkey)
		db.ops = append(db.ops, dbOp{op: delOpIgnoreExists, key: tombstoneKey})
		db.ops = append(db.ops, dbOp{op: putOp, key: key, value: value})
		if err != nil {
			dvid.Criticalf("Error on data put: %v\n", err)
			err = fmt.Errorf("Error on data put: %v", err)
		}
	}

	return err

}

// PutCallback writes a value with given key in a possibly versioned context and call the callback.
func (db *goBuffer) PutCallback(ctx storage.Context, tkey storage.TKey, value []byte, callback func()) error {
	if db == nil {
		return fmt.Errorf("Can't call Put() on nil Google Bucket")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in Put()")
	}

	var err error
	err = nil
	key := ctx.ConstructKey(tkey)
	if !ctx.Versioned() {
		db.ops = append(db.ops, dbOp{op: putOpCallback, key: key, value: value, callback: callback})
	} else {
		vctx, ok := ctx.(storage.VersionedCtx)
		if !ok {
			return fmt.Errorf("Non-versioned context that says it's versioned received in Put(): %v", ctx)
		}
		tombstoneKey := vctx.TombstoneKey(tkey)
		db.ops = append(db.ops, dbOp{op: delOpIgnoreExists, key: tombstoneKey})
		db.ops = append(db.ops, dbOp{op: putOpCallback, key: key, value: value, callback: callback})
		if err != nil {
			dvid.Criticalf("Error on data put: %v\n", err)
			err = fmt.Errorf("Error on data put: %v", err)
		}
	}

	return err

}

// RawPut is a low-level function that puts a key-value pair using full keys.
// This can be used in conjunction with RawRangeQuery.
func (db *goBuffer) RawPut(k storage.Key, v []byte) error {
	if db == nil {
		return fmt.Errorf("Can't call Put() on nil Google Bucket")
	}

	db.ops = append(db.ops, dbOp{op: putOp, key: k, value: v})
	return nil
}

// Delete deletes a key-value pair so that subsequent Get on the key returns nil.
func (db *goBuffer) Delete(ctx storage.Context, tkey storage.TKey) error {
	if db == nil {
		return fmt.Errorf("Can't call Delete on nil LevelDB")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in Delete()")
	}

	key := ctx.ConstructKey(tkey)
	if !ctx.Versioned() {
		db.ops = append(db.ops, dbOp{op: delOp, key: key})
	} else {
		vctx, ok := ctx.(storage.VersionedCtx)
		if !ok {
			return fmt.Errorf("Non-versioned context that says it's versioned received in Delete(): %v", ctx)
		}
		db.ops = append(db.ops, dbOp{op: delOp, key: key})
		tombstoneKey := vctx.TombstoneKey(tkey)
		db.ops = append(db.ops, dbOp{op: putOp, key: tombstoneKey, value: dvid.EmptyValue()})
	}

	return nil
}

// RawDelete is a low-level function.  It deletes a key-value pair using full keys
// without any context.  This can be used in conjunction with RawRangeQuery.
func (db *goBuffer) RawDelete(fullKey storage.Key) error {
	if db == nil {
		return fmt.Errorf("Can't call RawDelete on nil LevelDB")
	}

	db.ops = append(db.ops, dbOp{op: delOp, key: fullKey})
	return nil
}

// Put key-value pairs.  This is currently executed with parallel requests.
func (db *goBuffer) PutRange(ctx storage.Context, kvs []storage.TKeyValue) error {
	if db == nil {
		return fmt.Errorf("Can't call PutRange() on nil Google Bucket")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in PutRange()")
	}

	for _, kv := range kvs {
		db.Put(ctx, kv.K, kv.V)
	}

	return nil
}

// DeleteRange removes all key-value pairs with keys in the given range.
func (db *goBuffer) DeleteRange(ctx storage.Context, TkBeg, TkEnd storage.TKey) error {
	if db == nil {
		return fmt.Errorf("Can't call DeleteRange() on nil Google bucket")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in DeleteRange()")
	}
	db.ops = append(db.ops, dbOp{delRangeOp, nil, nil, TkBeg, TkEnd, nil, nil, nil})

	return nil
}

// Flush the buffer
func (buffer *goBuffer) Flush() error {
	retVals := make(chan error, len(buffer.ops))
	// limits the number of simultaneous requests (should this be global)
	workQueue := make(chan interface{}, MAXCONNECTIONS)

	for _, operation := range buffer.ops {
		workQueue <- nil
		go func(opdata dbOp) {
			defer func() {
				<-workQueue
			}()
			var err error
			err = nil
			if opdata.op == delOp {
				err = buffer.db.deleteV(opdata.key)
			} else if opdata.op == delOpIgnoreExists {
				buffer.db.deleteV(opdata.key)
			} else if opdata.op == delRangeOp {
				err = buffer.deleteRangeLocal(buffer.ctx, opdata.tkBeg, opdata.tkEnd, workQueue)
			} else if opdata.op == putOp {
				err = buffer.db.putV(opdata.key, opdata.value)
				storage.StoreKeyBytesWritten <- len(opdata.key)
				storage.StoreValueBytesWritten <- len(opdata.value)
			} else if opdata.op == putOpCallback {
				err = buffer.db.putV(opdata.key, opdata.value)
				storage.StoreKeyBytesWritten <- len(opdata.key)
				storage.StoreValueBytesWritten <- len(opdata.value)
				opdata.callback()
			} else if opdata.op == getOp {
				err = buffer.processRangeLocal(buffer.ctx, opdata.tkBeg, opdata.tkEnd, opdata.chunkop, opdata.chunkfunc, workQueue)
			} else {
				err = fmt.Errorf("Incorrect buffer operation specified")
			}

			// add errors to queue
			retVals <- err
		}(operation)
	}

	// check return values
	for range buffer.ops {
		err := <-retVals
		if err != nil {
			return err
		}
	}

	return nil
}

// deleteRangeLocal implements DeleteRange but with workQueue awareness.
func (db *goBuffer) deleteRangeLocal(ctx storage.Context, TkBeg, TkEnd storage.TKey, workQueue chan interface{}) error {
	if db == nil {
		return fmt.Errorf("Can't call DeleteRange() on nil Google bucket")
	}
	if ctx == nil {
		return fmt.Errorf("Received nil context in DeleteRange()")
	}

	// get all the keys within range, latest version, no tombstone
	keys, err := db.db.getKeysInRange(ctx, TkBeg, TkEnd)
	if err != nil {
		return err
	}

	// hackish -- release resource
	<-workQueue

	// wait for all deletes to complete
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		// use available threads
		workQueue <- nil
		go func(lkey storage.Key) {
			defer func() {
				<-workQueue
				wg.Done()
			}()
			tk, _ := ctx.TKeyFromKey(lkey)
			db.Delete(ctx, tk)
		}(key)
	}
	wg.Wait()

	// hackish -- reask for resource
	workQueue <- nil

	return nil
}

// processRangeLocal implements ProcessRange functionality but with workQueue awareness
func (db *goBuffer) processRangeLocal(ctx storage.Context, TkBeg, TkEnd storage.TKey, op *storage.ChunkOp, f storage.ChunkFunc, workQueue chan interface{}) error {
	// grab keys
	keys, _ := db.db.getKeysInRange(ctx, TkBeg, TkEnd)

	// process keys in parallel
	kvmap := make(map[string][]byte)
	for _, key := range keys {

		kvmap[string(key)] = nil
	}

	// hackish -- release resource
	<-workQueue

	var wg sync.WaitGroup
	for _, key := range keys {
		// use available threads
		workQueue <- nil
		wg.Add(1)
		go func(lkey storage.Key) {
			defer func() {
				<-workQueue
				wg.Done()
			}()
			value, err := db.db.getV(lkey)
			if value == nil || err != nil {
				kvmap[string(lkey)] = nil
			} else {
				kvmap[string(lkey)] = value
			}

		}(key)

	}
	wg.Wait()

	// hackish -- reask for resource
	workQueue <- nil

	var err error
	err = nil
	// return keyvalues
	for key, val := range kvmap {
		tk, err := ctx.TKeyFromKey(storage.Key(key))
		if err != nil {
			return err
		}

		if val == nil {
			return fmt.Errorf("Could not retrieve value")
		}

		if op != nil && op.Wg != nil {
			op.Wg.Add(1)
		}

		tkv := storage.TKeyValue{tk, val}
		chunk := &storage.Chunk{op, &tkv}
		if err := f(chunk); err != nil {
			return err
		}
	}

	return err
}

// --- Helper function ----

func grabPrefix(key1 storage.Key, key2 storage.Key) storage.Key {
	var prefixe storage.Key
	key1m := base64.URLEncoding.EncodeToString(key1)
	key2m := base64.URLEncoding.EncodeToString(key2)
	for spot := range key1m {
		if key1m[spot] != key2m[spot] {
			break
		}
		prefixe = append(prefixe, key1m[spot])
	}
	prefix, _ := base64.URLEncoding.DecodeString(string(prefixe))
	return prefix
}

type KeyArray []storage.Key

func (k KeyArray) Less(i, j int) bool {
	return string(k[i]) < string(k[j])
}

func (k KeyArray) Swap(i, j int) {
	k[i], k[j] = k[j], k[i]
}

func (k KeyArray) Len() int {
	return len(k)
}