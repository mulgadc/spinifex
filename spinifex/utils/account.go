package utils

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/nats-io/nats.go"
)

// VersionKey is the well-known KV key used to store a bucket's schema version.
const VersionKey = "_version"

// WriteVersion writes the schema version to a bucket, only if missing or older.
// Returns an error if the stored value is corrupt (non-integer).
func WriteVersion(kv nats.KeyValue, version int) error {
	entry, err := kv.Get(VersionKey)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return fmt.Errorf("read current version: %w", err)
	}
	if err == nil {
		stored, parseErr := strconv.Atoi(string(entry.Value()))
		if parseErr != nil {
			return fmt.Errorf("corrupted _version key (raw=%q): %w", string(entry.Value()), parseErr)
		}
		if stored >= version {
			return nil
		}
	}
	_, err = kv.PutString(VersionKey, strconv.Itoa(version))
	return err
}

// ReadVersion reads the schema version from a bucket; returns 0 if not set.
// Errors distinguish network failures and corrupt values from "not set".
func ReadVersion(kv nats.KeyValue) (int, error) {
	entry, err := kv.Get(VersionKey)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("read version: %w", err)
	}
	v, parseErr := strconv.Atoi(string(entry.Value()))
	if parseErr != nil {
		return 0, fmt.Errorf("corrupted _version key (raw=%q): %w", string(entry.Value()), parseErr)
	}
	return v, nil
}

// GlobalAccountID is the root/system account ID.
const GlobalAccountID = "000000000000"

// AccountKey returns a KV key scoped to an account: "{accountID}.{resourceID}".
func AccountKey(accountID, resourceID string) string {
	return accountID + "." + resourceID
}

// IsAccountID checks if a string is a valid 12-digit AWS account ID.
func IsAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// GetOrCreateKVBucket creates or retrieves a NATS KV bucket.
func GetOrCreateKVBucket(js nats.JetStreamContext, bucketName string, history int) (nats.KeyValue, error) {
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  bucketName,
		History: SafeIntToUint8(history),
	})
	if err != nil {
		kv, err = js.KeyValue(bucketName)
		if err != nil {
			return nil, err
		}
	}
	return kv, nil
}

// DeleteKVBucketIfExists deletes a NATS KV bucket by name, returning nil if it does not exist.
func DeleteKVBucketIfExists(js nats.JetStreamContext, bucketName string) error {
	err := js.DeleteKeyValue(bucketName)
	if err == nil {
		return nil
	}
	if errors.Is(err, nats.ErrBucketNotFound) || errors.Is(err, nats.ErrStreamNotFound) {
		return nil
	}
	return err
}
