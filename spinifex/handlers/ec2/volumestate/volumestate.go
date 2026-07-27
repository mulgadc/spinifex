// Package volumestate reads and writes the control-plane-owned attachment
// state for an EBS volume, persisted to a per-volume state.json object.
//
// It is kept out of config.json because config.json is rewritten by the live
// nbdkit VB on every SaveState (clobbering any State the control plane wrote
// there) and is a sealed object for encrypted volumes (a second writer reuses
// the AES-GCM nonce). state.json is plaintext, viperblock never touches it, so
// the control plane is its single writer. Readers outside the volume service
// (the snapshot path) need the same record, which is why it lives here.
package volumestate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// Record is the control-plane-owned attachment state of a volume.
type Record struct {
	State            string    `json:"state"`
	AttachedInstance string    `json:"attachedInstance"`
	DeviceName       string    `json:"deviceName"`
	AttachedAt       time.Time `json:"attachedAt"`
}

// Key is the S3 key for a volume's control-plane state object.
func Key(volumeID string) string { return volumeID + "/state.json" }

// Write persists the attachment state, replacing any previous record.
func Write(ctx context.Context, store objectstore.ObjectStore, bucket, volumeID string, rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal volume state: %w", err)
	}
	_, err = store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(Key(volumeID)),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to write volume state to S3: %w", err)
	}
	return nil
}

// Read returns the attachment state. found=false with a nil error means the
// object is absent (a volume predating the state.json split), in which case the
// caller falls back to the State embedded in config.json.
func Read(ctx context.Context, store objectstore.ObjectStore, bucket, volumeID string) (Record, bool, error) {
	getResult, err := store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(Key(volumeID)),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("failed to get volume state: %w", err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return Record{}, false, fmt.Errorf("failed to read volume state body: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		return Record{}, false, fmt.Errorf("failed to unmarshal volume state: %w", err)
	}
	return rec, true, nil
}
