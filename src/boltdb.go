package main

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/boltdb/bolt"
	bloom "github.com/pmylund/go-bloom"
)

type BloomFilterKeys struct {
	cache     *bloom.CountingFilter
	bloomLock *sync.RWMutex
}

func NewBloomFilterKeys(maxKeysPerBucket int) *BloomFilterKeys {
	me := BloomFilterKeys{cache: nil, bloomLock: &sync.RWMutex{}}
	me.cache = bloom.NewCounting(maxKeysPerBucket, 0.01)
	return &me
}

func (bf BloomFilterKeys) Add(key []byte) {
	bf.bloomLock.Lock()
	bf.cache.Add(key)
	bf.bloomLock.Unlock()
}

func (bf BloomFilterKeys) Remove(key []byte) {
	bf.bloomLock.Lock()
	bf.cache.Remove(key)
	bf.bloomLock.Unlock()
}

func (bf BloomFilterKeys) Reset() {
	bf.bloomLock.Lock()
	bf.cache.Reset()
	bf.bloomLock.Unlock()
}

func (bf BloomFilterKeys) Test(key []byte) bool {
	bf.bloomLock.RLock()
	r := bf.cache.Test(key)
	bf.bloomLock.RUnlock()
	return r
}

type InternalValue struct {
	key        []byte
	flags      int32
	expiration int
	cas        int64
	value      []byte
}

type KVBoltDBBackend struct {
	filename         string
	bucketName       string
	db               *bolt.DB
	expirationdb     *bolt.DB
	keyCache         map[string]*BloomFilterKeys
	maxKeysPerBucket int
}

func NewKVBoltDBBackend(filename string, bucketName string, maxKeysPerBucket int) (*KVBoltDBBackend, error) {
	var err error
	b := KVBoltDBBackend{filename: filename, bucketName: bucketName, db: nil, expirationdb: nil, keyCache: nil, maxKeysPerBucket: maxKeysPerBucket}
	b.db, err = bolt.Open(filename, 0644, nil)
	if err != nil {
		return nil, err
	}

	b.keyCache = make(map[string]*BloomFilterKeys)
	b.keyCache[bucketName] = NewBloomFilterKeys(maxKeysPerBucket)

	err = b.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(b.bucketName))
		if bucket == nil {
			return fmt.Errorf("Bucket %q not found!", b.bucketName)
		}
		bucket.ForEach(func(k, v []byte) error {
			b.keyCache[bucketName].Add(k)
			return nil
		})
		return nil
	})
	return &b, nil
}

func (be KVBoltDBBackend) Set(key []byte, value []byte) error {
	return be.Put(key, value, false, true)
}

// store data only if the server doesnt holds it yet
func (be KVBoltDBBackend) Add(key []byte, value []byte) error {
	return be.Put(key, value, false, false)
}

// store data only if the server already holds this key
func (be KVBoltDBBackend) Replace(key []byte, value []byte) error {
	return be.Put(key, value, true, false)
}

// INCR data, yields error if the represented value doesnt maps to int. Starts from 0, no negative values
func (be KVBoltDBBackend) Incr(key []byte, value uint) (int, error) {
	return be.Increment(key, int(value), false)
}

// DECR data, yields error if the represented value doesnt maps to int. Stops at 0, no negative values
func (be KVBoltDBBackend) Decr(key []byte, value uint) (int, error) {
	return be.Increment(key, int(value)*-1, false)
}

// Generic get and set for incr/decr tx
func (be KVBoltDBBackend) Increment(key []byte, value int, create_if_not_exists bool) (int, error) {
	var ret int
	err := be.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(be.bucketName))

		if err != nil {
			return err
		}

		bf := be.keyCache[be.bucketName].Test(key)
		if bf == false {
			if create_if_not_exists == false {
				return fmt.Errorf("Increment: Key %s exists", string(key))
			}
			i := string(0 + value)
			err := bucket.Put(key, []byte(i))
			if err != nil {
				return fmt.Errorf("Error storing incr/decr value for key %s - %s", string(key), i)
			}
			ret = 0 + value
		} else {
			v := bucket.Get(key)
			i, err := strconv.Atoi(string(v))
			if err != nil {
				return fmt.Errorf("Data cannot be incr/decr for key %s - %s", string(key), string(v))
			}
			i = i + value
			s := fmt.Sprintf("%d", i)
			err = bucket.Put(key, []byte(s))
			if err != nil {
				return fmt.Errorf("Error storing incr/decr value for key %s - %d", string(key), i)
			}
			ret = i
		}
		return nil
	})
	return ret, err
}

func (be KVBoltDBBackend) Put(key []byte, value []byte, replace bool, passthru bool) error {
	err := be.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(be.bucketName))

		if err != nil {
			return err
		}
		if passthru == false {
			if replace == true {
				bf := be.keyCache[be.bucketName].Test(key)
				if bf == false {
					v := bucket.Get(key)
					if v == nil {
						return fmt.Errorf("Key %s do not exists, replace set to true", string(key))
					}
				}
			} else {
				bf := be.keyCache[be.bucketName].Test(key)
				if bf == true {
					v := bucket.Get(key)
					if v != nil {
						return fmt.Errorf("Key %s exists, replace set to false", string(key))
					}
				}
			}
		}

		be.keyCache[be.bucketName].Add(key)
		err = bucket.Put(key, value)
		if err != nil {
			return err
		}

		return nil
	})

	return err
}

func (be KVBoltDBBackend) Get(key []byte) ([]byte, error) {
	var val []byte
	bf := be.keyCache[be.bucketName].Test(key)
	if bf == false {
		return nil, nil
	}
	err := be.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(be.bucketName))
		if bucket == nil {
			return fmt.Errorf("Bucket %q not found!", be.bucketName)
		}

		val = bucket.Get(key)
		return nil
	})

	if err != nil {
		return nil, err
	}
	return val, nil

}

// returns deleted, error
func (be KVBoltDBBackend) Delete(key []byte, only_if_exists bool) (bool, error) {
	if only_if_exists == true {
		x, err := be.Get(key)
		if err != nil {
			return false, err
		}
		if x == nil {
			return false, nil
		}
	}
	err := be.db.Update(func(tx *bolt.Tx) error {
		be.keyCache[be.bucketName].Remove(key)
		return tx.Bucket([]byte(be.bucketName)).Delete(key)
	})
	return true, err
}

func (be KVBoltDBBackend) Flush() error {
	be.db.Update(func(tx *bolt.Tx) error {
		be.keyCache[be.bucketName].Reset()
		return tx.DeleteBucket([]byte(be.bucketName))
	})
	return nil
}

func (be KVBoltDBBackend) BucketStats() error { return nil }
func (be KVBoltDBBackend) Close() {
	be.db.Close()
}
func (be KVBoltDBBackend) GetDbPath() string {
	return be.filename
}

func (be KVBoltDBBackend) SwitchBucket(bucket string) {
	if be.keyCache[bucket] == nil {
		//be.keyCache[bucket] = NewMemcachedKeys()
		be.keyCache[bucket] = NewBloomFilterKeys(be.maxKeysPerBucket)
	}
	be.bucketName = bucket
}

func (be KVBoltDBBackend) Range(key []byte, limit int, from []byte, reverse bool) (map[string][]byte, error) {
	return nil, nil
}

func (be KVBoltDBBackend) Stats() string {
	return ""
}
