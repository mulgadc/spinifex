package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	migrationstore "github.com/mulgadc/spinifex/spinifex/domains/migration/store"
	"github.com/nats-io/nats.go/jetstream"
)

// migrationDrain is level 4's rollback drain: migration buckets and records,
// which an older build's termination engine cannot reach.
type migrationDrain struct {
	mu    sync.Mutex
	count func(ctx context.Context) (int, error)
	out   io.Writer
}

var migDrain = &migrationDrain{}

func (d *migrationDrain) connect(js jetstream.JetStream, out io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.out = out
	d.count = func(ctx context.Context) (int, error) { return migrationstore.CountAll(ctx, js) }
}

func (d *migrationDrain) check(ctx context.Context) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == nil {
		return 0, errors.New("migration drain is not connected")
	}
	n, err := d.count(ctx)
	if err != nil {
		return 0, fmt.Errorf("count migration records: %w", err)
	}
	if d.out != nil {
		fmt.Fprintf(d.out, "Migration: %d record(s).\n", n)
		if n > 0 {
			fmt.Fprintln(d.out, "Delete every migration connection, assessment and copy task before rolling back: rollback never deletes one.")
		}
	}
	return n, nil
}
