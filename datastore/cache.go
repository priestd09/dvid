/*
	This file holds caching and buffering for datastore operation.
*/

package datastore

import (
	"encoding/json"
	"fmt"
	"log"
	_ "os"
	"sync"
	"time"

	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/keyvalue"
)

// Constants that allow tuning of DVID for a particular target computer.
const (
	// Default size of LRU cache for DVID data blocks in MB.  The larger this number,
	// the fewer disk accesses you'll need.
	DefaultCacheMBytes = 512

	// Default number of block handlers to use per data type. 
	DefaultNumBlockHandlers = 8

	// NumBlockHandlers sets the number of processors we have for our "map" operation.
	NumBlockHandlers = 8

	// Number of block write requests that can be buffered on each block handler
	// before sender is blocked.  
	// This constant * sizeof(request struct) * NumBlockHandlers
	// should be a reasonable number for the target DVID computer.
	// The request struct contains two pointers.
	BlockHandlerBufferSize = 100000
)

type OpType uint8

const (
	GetOp OpType = iota
	PutOp
)

func (op OpType) String() string {
	switch op {
	case GetOp:
		return "GET"
	case PutOp:
		return "PUT"
	}
	return fmt.Sprintf("Illegal Op (%d)", op)
}

type OpResult string

// Block is the unit of get/put for each data type.  We typically decompose a
// larger structure (DataStruct) into Blocks, process each Block separately
// by a handler assigned for each spatial index, and then let the requestor 
// know when all the processing is done via a sync.WaitGroup.
type BlockRequest struct {
	// The larger data structure that we're going to fill in using blocks.
	// This may be a slice and thinner than the blocks it intersects.
	DataStruct

	// Block holds the data for a block, a small rectangular volume of voxels.
	Block keyvalue.Value

	// Parameters for this particular block
	Op         OpType
	SpatialKey SpatialIndex
	BlockKey   keyvalue.Key

	// Let's us notify requestor when all blocks are done.
	Wait *sync.WaitGroup

	DB keyvalue.KeyValueDB

	// Include a WriteBatch so PUT ops can be batched
	//WriteBatch keyvalue.WriteBatch
}

// Each data type has a pool of channels to communicate with block handlers. 
type BlockChannels map[DataSetString]([]chan *BlockRequest)

// Track requested/completed block ops
type loadStruct struct {
	Requests  int
	Completed int
}
type loadMap map[DataSetString]loadStruct

var (
	// HandlerChannels are map from data type names to a pool of block handler
	// goroutines.  See the function ReserveBlockHandlers.
	HandlerChannels BlockChannels

	// DiskAccess is a mutex to make sure we don't have goroutines simultaneously trying
	// to access the key-value database on disk.
	// TODO: Reexamine this in the context of parallel disk drives during cluster use.
	DiskAccess sync.Mutex

	// Monitor the requested and completed block ops
	loadLastSec    loadMap
	loadAccess     sync.RWMutex
	doneChannel    chan DataSetString
	requestChannel chan DataSetString
)

func init() {
	HandlerChannels = make(BlockChannels)
	loadLastSec = make(loadMap)
	doneChannel = make(chan DataSetString)
	requestChannel = make(chan DataSetString)
	go loadMonitor()
}

// Monitors the # of requests/done on block handlers per data set.
func loadMonitor() {
	secondTick := time.Tick(1 * time.Second)
	requests := make(map[DataSetString]int)
	completed := make(map[DataSetString]int)
	for {
		select {
		case name := <-doneChannel:
			completed[name]++
		case name := <-requestChannel:
			requests[name]++
		case <-secondTick:
			loadAccess.RLock()
			for name, _ := range loadLastSec {
				loadLastSec[name] = loadStruct{
					Requests:  requests[name],
					Completed: completed[name],
				}
				requests[name] = 0
				completed[name] = 0
			}
			loadAccess.RUnlock()
		}
	}
}

// ReserveBlockHandlers makes sure we have block handler goroutines for each
// data set.  Blocks are routed to the same handler each time, so concurrent
// access to a block by multiple requests funneled sequentially into a handler.
func ReserveBlockHandlers(name DataSetString, t TypeService) {
	loadAccess.Lock()
	loadLastSec[name] = loadStruct{}
	loadAccess.Unlock()

	var channelMapAccess sync.Mutex
	channelMapAccess.Lock()
	// Do we have channels and handlers for this type and image version?
	_, found := HandlerChannels[name]
	if !found {
		log.Printf("Starting %d block handlers for data set '%s' (%s)...\n",
			t.NumBlockHandlers(), name, t.TypeName())
		channels := make([]chan *BlockRequest, 0, t.NumBlockHandlers())
		for i := 0; i < t.NumBlockHandlers(); i++ {
			channel := make(chan *BlockRequest, BlockHandlerBufferSize)
			channels = append(channels, channel)
			go func(i int, c chan *BlockRequest) {
				dvid.Log(dvid.Debug, "Starting block handler %d for %s...",
					i+1, name)
				for {
					block := <-c
					if block == nil {
						log.Fatalln("Received nil block in block handler!")
					}
					//dvid.Fmt(dvid.Debug, "Running handler on block %x...\n", block.SpatialKey)
					block.DataStruct.BlockHandler(block)
					doneChannel <- name
				}
			}(i, channel)
			// TODO -- keep stats on # of handlers
		}
		HandlerChannels[name] = channels
	}
	channelMapAccess.Unlock()
}

// BlockLoadJSON returns a JSON description of the block requests for each dataset.
func BlockLoadJSON() (jsonStr string, err error) {
	loadAccess.RLock()
	m, err := json.Marshal(loadLastSec)
	loadAccess.RUnlock()
	if err != nil {
		return
	}
	jsonStr = string(m)
	return
}

// MapBlocks breaks down a DataStruct into a sequence of blocks that can be
// efficiently read from the key-value database.  It then passes those blocks
// to datatype-specific block handlers that read from preallocated channels.
//
// Phase 1: Time leveldb built-in LRU cache and write buffer. (current)
// Phase 2: Minimize leveldb built-in LRU cache and use DVID LRU cache with
//   periodic and on-demand writes. 
// TODO -- Examine possible interleaving of block-level requests across MapBlocks()
//   calls and its impact on GET requests fulfilled while some blocks are still being
//   modified.
func (vs *VersionService) MapBlocks(op OpType, data DataStruct, wg *sync.WaitGroup) error {

	// Get components of the block key
	uuidBytes := vs.UuidBytes()
	datatypeBytes, err := vs.DataIndexBytes(data.DataSetName())
	if err != nil {
		return err
	}

	// Make sure we have Block Handlers for this data type.
	channels, found := HandlerChannels[data.DataSetName()]
	if !found {
		return fmt.Errorf("Error in reserving block handlers in MapBlocks() for %s!",
			data.DataSetName())
	}

	// Traverse blocks, get key/values if not in cache, and put block in queue for handler.
	ro := keyvalue.NewReadOptions()
	db_it, err := vs.kvdb.NewIterator(ro)
	defer db_it.Close()
	if err != nil {
		return err
	}
	spatial_it := NewSpatialIterator(data)
	start := true

	//dvid.Fmt(dvid.Debug, "Mapping blocks for %s\n", data)
	DiskAccess.Lock()
	switch op {
	case PutOp, GetOp:
		for {
			spatialBytes := spatial_it()
			if spatialBytes == nil {
				break
			}
			blockKey := BlockKey(uuidBytes, spatialBytes, datatypeBytes, data.IsolatedKeys())

			// Pull from the datastore
			if start || (db_it.Valid() && string(db_it.Key()) < string(blockKey)) {
				db_it.Seek(blockKey)
				start = false
			}
			var value keyvalue.Value
			if db_it.Valid() && string(db_it.Key()) == string(blockKey) {
				value = db_it.Value()
				db_it.Next()
			} else {
				if op == PutOp {
					value = make(keyvalue.Value, data.BlockBytes(), data.BlockBytes())
				} else {
					continue // If have no value, simple use zero value of slice/subvolume.
				}
			}

			// Initialize the block request
			req := &BlockRequest{
				DataStruct: data,
				Block:      value,
				Op:         op,
				SpatialKey: SpatialIndex(spatialBytes),
				BlockKey:   blockKey,
				Wait:       wg,
				DB:         vs.kvdb,
				//WriteBatch: writeBatch,
			}

			// Try to spread sequential block keys among different block handlers to get 
			// most out of our concurrent processing.
			if wg != nil {
				wg.Add(1)
			}
			channelNum := req.SpatialKey.Hash(data, len(channels))
			//dvid.Fmt(dvid.Debug, "Sending %s block %s request %s down channel %d\n",
			//	op, SpatialIndex(spatialBytes).BlockCoord(data), data, channelNum)
			channels[channelNum] <- req
			requestChannel <- data.DataSetName()
		}
	default:
		return fmt.Errorf("Illegal operation (%d) asked for in MapBlocks()", op)
	}
	DiskAccess.Unlock()
	return nil
}