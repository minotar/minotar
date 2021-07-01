package bolt_cache

import (
	"errors"
	"fmt"
	"time"

	"github.com/boltdb/bolt"
	"github.com/minotar/imgd/pkg/cache"
	store_expiry "github.com/minotar/imgd/pkg/cache/util/expiry/store"
	"github.com/minotar/imgd/pkg/storage/bolt_store"
)

const (
	// Minimum Duration between full bucket scans looking for expired keys
	COMPACTION_SCAN_INTERVAL = 5 * time.Minute
	// Max number of keys to scan in a single DB Transaction
	COMPACTION_MAX_SCAN = 50000
)

var ErrCompactionInterupted = errors.New("Compaction was interupted")
var ErrCompactionFinished = errors.New("Compaction has finished")

type BoltCache struct {
	*bolt_store.BoltStore
	*store_expiry.StoreExpiry
}

// ensure that the cache.Cache interface is implemented
var _ cache.Cache = new(BoltCache)

func NewBoltCache(path, name string) (*BoltCache, error) {
	bs, err := bolt_store.NewBoltStore(path, name)
	if err != nil {
		return nil, err
	}
	bc := &BoltCache{BoltStore: bs}

	// Create a StoreExpiry using the BoltCache method
	se, err := store_expiry.NewStoreExpiry(bc.ExpiryScan, COMPACTION_SCAN_INTERVAL)
	if err != nil {
		return nil, err
	}
	bc.StoreExpiry = se

	return bc, nil
}

func (bc *BoltCache) Insert(key string, value []byte) error {
	return bc.InsertTTL(key, value, 0)
}

func (bc *BoltCache) InsertTTL(key string, value []byte, ttl time.Duration) error {
	bse := bc.NewStoreEntry(key, value, ttl)
	keyBytes, valueBytes := bse.Encode()
	err := bc.DB.Update(func(tx *bolt.Tx) error {
		//e := tx.Bucket([]byte(bc.nameExpiry))
		b := tx.Bucket([]byte(bc.Name))
		return b.Put(keyBytes, valueBytes)
	})
	if err != nil {
		return fmt.Errorf("Inserting \"%s\" into \"%s\": %s", key, bc.Name, err)
	}
	return nil
}

func (bc *BoltCache) InsertBatch(key string, value []byte) error {
	return fmt.Errorf("Not implemented")
}

func (bc *BoltCache) retrieveBSE(key string) (store_expiry.StoreEntry, error) {
	var bse store_expiry.StoreEntry
	keyBytes := []byte(key)
	err := bc.DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bc.Name))
		v := b.Get(keyBytes)
		if v == nil {
			return cache.ErrNotFound
		}
		// Set byte slice length for copy
		data := make([]byte, len(v))
		copy(data, v)
		bse = store_expiry.DecodeStoreEntry(keyBytes, data)
		return nil
	})
	if err == cache.ErrNotFound {
		return bse, err
	} else if err != nil {
		return bse, fmt.Errorf("Retrieving \"%s\" from \"%s\": %s", key, bc.Name, err)
	}

	// We could at this stage Remove Expired entries - but returning expired is better
	return bse, nil
}

func (bc *BoltCache) Retrieve(key string) ([]byte, error) {
	bse, err := bc.retrieveBSE(key)
	if err != nil {
		return nil, err
	}

	// Optionally we could further check expiry here
	// (though general preference for us is to return stale)
	return bse.Value, nil
}

// TTL returns an error if the key does not exist, or it has no expiry
// Otherwise return a TTL (always at least 1 Second per `StoreExpiry`)
func (bc *BoltCache) TTL(key string) (time.Duration, error) {
	bse, err := bc.retrieveBSE(key)
	if err != nil {
		return 0, err
	}

	return bse.TTL(bc.StoreExpiry.Clock.Now())
}

func (bc *BoltCache) Remove(key string) error {
	err := bc.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bc.Name))
		return b.Delete([]byte(key))
	})
	if err != nil {
		return fmt.Errorf("Removing \"%s\" from \"%s\": %s", key, bc.Name, err)
	}
	return nil
}

func firstOrSeek(c *bolt.Cursor, keyMarker string) (k, v []byte) {
	if keyMarker == "" {
		return c.First()
	} else {
		return c.Seek([]byte(keyMarker))
	}
}

func (bc *BoltCache) expiryScan(reviewTime time.Time, chunkSize int) {
	var scannedCount, expiredCount int
	var keyMarker string
	var scanErr error
	dbLength := int(bc.Len())

	// Keep scanning until it's interupted or finishes (via a return)
	// Crucially, this loop ensures that we aren't holding a Write lock for an extended time
	for {
		if keyMarker != "" {
			fmt.Printf("kM starts as %s\n", keyMarker)
		}
		// Start a transaction for processing a chunk of keys
		// Technically, keys might be added/removed between transactions
		_ = bc.DB.Update(func(tx *bolt.Tx) error {
			c := tx.Bucket([]byte(bc.Name)).Cursor()

			var i int
			var k, v []byte
			// Loop through each key/value until we've reached the max chunk scan size
			// Either start at beginning, or use the marker if it's set
			for k, v = firstOrSeek(c, keyMarker); i < chunkSize; k, v = c.Next() {
				// Check on every key that the cache is still running/not stopping
				if !bc.IsRunning() {
					// Todo: debug logging
					fmt.Printf("Caching is no longer running, exiting ExpiryScan\n")
					scanErr = ErrCompactionInterupted
					return nil
				}

				if k == nil {
					// Todo: debug logging
					fmt.Printf("Nil Key means that all keys have been scanned\n")
					scanErr = ErrCompactionFinished
					return nil
				}
				// Increment counters for stats and chunk max
				scannedCount++
				i++
				fmt.Printf("Scanned %s\n", k)

				if store_expiry.HasBytesExpired(v[:4], reviewTime) {
					err := c.Delete()
					// Todo: It seems this advances the cursor????
					fmt.Printf("Deleted: %s\n", k)
					if err != nil {
						// Todo: logging
						fmt.Printf("Error Removing Expired \"%s\" from \"%s\": %s\n", string(k), bc.Name, err)
					}
					expiredCount++
				}
			}

			// More keys to be scanned
			// The for loop above would have assigned k to the next key, but not scanned it
			// We now use that assignment to set the Marker for the next iteration
			keyMarker = string(k)
			fmt.Printf("kM ends as %s\n", keyMarker)
			if k == nil {
				// Todo: debug logging
				fmt.Printf("Nil Key means that all keys have been scanned\n")
				scanErr = ErrCompactionFinished
				return nil
			}
			return nil
			// end of DB transaction
		})
		// We need to check scanErr for globally set errors/state

		// Todo: Merge all this??
		if scanErr == ErrCompactionInterupted {
			// Todo: info logging
			fmt.Printf("Caching is %+v, exiting ExpiryScan. Len: %d, Scanned: %d, Expired: %d\n", bc.IsRunning(), dbLength, scannedCount, expiredCount)
			return
		} else if scanErr == ErrCompactionFinished {
			// Todo: info logging
			fmt.Printf("All keys have been scanned. Len: %d, Scanned: %d, Expired: %d\n", dbLength, scannedCount, expiredCount)
			return
		}
	}

}

// Ran on interval by the StoreExpiry
func (bc *BoltCache) ExpiryScan() {
	bc.expiryScan(bc.StoreExpiry.Clock.Now(), COMPACTION_MAX_SCAN)
}

/*

// Todo: Incomplete. Could be used when we want to maitain a DB length/size
func (bc *BoltCache) randomEvict(evictScan int) error {
	// The level of randomness isn't actually crucial - though getting a sample from a wide range is beneficial
	// Randomly grabbing a value from the DB seems difficult as there is no context of index based on ID.
	// A full scan would be hugely inefficient as well...
	// At a byte level, we know the first key is the "lowest" and the last is highest
	// We could try and `seek` to a random point between the lowest and highest.
	// That would probably be straightforward were it not for the issue of uneven key lengths
	// A way we can overcome that (while reducing the randomness...) is to perform a scan of some keys
	// at each length of slice between the First/Last key. Then generate a random byte slice
	// and try and seek to it and read the key.
	// Our random byte slices can probably prefer ascii characters
	// (Seeking an unknown key returns the next key after where it would be positioned)
	// Based on the returned values/expiry from the Seek, chooses the oldest key to Delete

	if dbLength := int(bc.Len()); dbLength < evictScan {
		evictScan = dbLength
	}

	err := bc.DB.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bc.Name)).Cursor()
		firstKey, _ := c.First()
		lastKey, _ := c.Last()
		fmt.Printf("First: %+v\nLast: %+v\n", firstKey, lastKey)
		_ = evictScan
		return nil
	})
	if err != nil {
		return fmt.Errorf("Evicting a random key: %s", err)
	}
	return nil
}

*/

func (bc *BoltCache) Start() {
	// Start the Expiry monitor/compactor
	bc.StoreExpiry.Start()
}

func (bc *BoltCache) Stop() {
	// Start the Expiry monitor/compactor
	bc.StoreExpiry.Stop()
}

func (bc *BoltCache) Close() {
	bc.Stop()
	bc.BoltStore.Close()
}