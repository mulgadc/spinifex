package qmpcollector

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// collector reconciles one poller per discovered VM metadata file.
type collector struct {
	cfg     *Config
	nc      *nats.Conn
	pollers map[string]*poller // keyed by instance ID
}

func newCollector(cfg *Config, nc *nats.Conn) *collector {
	return &collector{cfg: cfg, nc: nc, pollers: make(map[string]*poller)}
}

// run rescans the runtime dir until ctx cancels, then stops every poller.
func (c *collector) run(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.DiscoverInterval)
	defer ticker.Stop()
	c.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			for _, p := range c.pollers {
				p.stop()
			}
			return
		case <-ticker.C:
			c.reconcile(ctx)
		}
	}
}

// reconcile diffs metadata files against running pollers. A metadata file
// whose socket vanished is a leftover from an unclean QEMU exit: it is GC'd
// so terminated instances stop being polled.
func (c *collector) reconcile(ctx context.Context) {
	pattern := filepath.Join(c.cfg.RuntimeDir, utils.QMPTelemetryPrefix+"*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		slog.Error("qmp-collector: metadata glob failed", "pattern", pattern, "err", err)
		return
	}

	seen := make(map[string]bool, len(files))
	for _, f := range files {
		meta, err := readMeta(f)
		if err != nil {
			slog.Warn("qmp-collector: unreadable metadata, skipping", "file", f, "err", err)
			continue
		}
		if _, err := os.Stat(meta.Socket); errors.Is(err, fs.ErrNotExist) {
			slog.Info("qmp-collector: socket gone, removing stale metadata",
				"instanceId", meta.InstanceID, "socket", meta.Socket)
			_ = os.Remove(f)
			continue
		}
		seen[meta.InstanceID] = true
		if p, ok := c.pollers[meta.InstanceID]; ok {
			p.updateMeta(meta)
			continue
		}
		p := newPoller(c.cfg, c.nc, meta)
		c.pollers[meta.InstanceID] = p
		p.start(ctx)
	}

	for id, p := range c.pollers {
		if !seen[id] {
			p.stop()
			delete(c.pollers, id)
			slog.Info("qmp-collector: instance gone, poller stopped", "instanceId", id)
		}
	}
}

func readMeta(path string) (types.GuestTelemetryMeta, error) {
	var meta types.GuestTelemetryMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	if meta.InstanceID == "" || meta.Socket == "" {
		return meta, errors.New("metadata missing instance_id or socket")
	}
	return meta, nil
}

// staggerOffset spreads pollers over the period so a node's VMs never hit
// their sockets in the same second.
func staggerOffset(instanceID string, period time.Duration) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(instanceID))
	return time.Duration(h.Sum32()) % period
}

// labelKey canonicalizes a series identity for cache keying.
func labelKey(name string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(name)
	for _, k := range sortedKeys(labels) {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
