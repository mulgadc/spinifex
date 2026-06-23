package dns

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Writer is the control-plane DNS record writer. It owns the read-modify-write
// of zone TOML files in s3://northstar/ using the system predastore credentials.
type Writer struct {
	enabled    bool
	s3cfg      *nsconfig.S3Config
	baseDomain string
	localNS    nsconfig.NameserverSeed
	ttl        uint32
	nc         *nats.Conn
}

// NewWriter resolves the northstar S3 endpoint/bucket from the node's
// northstar.toml and pairs it with the system predastore credentials. The
// writer is disabled (a no-op) when northstar is not configured for S3. The
// NATS connection, when non-nil, is used to fan out per-zone reload events.
func NewWriter(cfg *config.Config, nc *nats.Conn) *Writer {
	w := &Writer{ttl: DefaultTTL, nc: nc}
	s3cfg, baseDomain, ok := zoneS3Config(cfg)
	if !ok {
		slog.Info("dns writer: northstar S3 not configured, record registration disabled")
		return w
	}
	w.enabled = true
	w.s3cfg = s3cfg
	w.baseDomain = baseDomain
	w.localNS = nsconfig.NameserverSeed{Host: "ns1", IP: localNameserverIP(cfg)}
	return w
}

// Enabled reports whether the writer will process changes.
func (w *Writer) Enabled() bool { return w.enabled }

// Subscribe registers the queue-group request-reply consumer. It is a no-op when
// the writer is disabled.
func (w *Writer) Subscribe(nc *nats.Conn) (*nats.Subscription, error) {
	if !w.enabled {
		return nil, nil
	}
	return nc.QueueSubscribe(SubjectRecordsetChange, QueueGroup, func(msg *nats.Msg) {
		utils.ServeNATSRequest(msg, w.ApplyBatch)
	})
}

// ApplyBatch applies a batch of changes, grouped per zone so each zone object is
// read-modified-written once.
func (w *Writer) ApplyBatch(batch *ChangeBatch) (*ChangeResult, error) {
	if !w.enabled {
		return nil, fmt.Errorf("dns writer disabled")
	}
	byZone := map[string][]Change{}
	order := []string{}
	for _, c := range batch.Changes {
		if _, seen := byZone[c.Zone]; !seen {
			order = append(order, c.Zone)
		}
		byZone[c.Zone] = append(byZone[c.Zone], c)
	}

	res := &ChangeResult{}
	for _, zone := range order {
		applied, err := w.applyZone(zone, byZone[zone])
		if err != nil {
			return nil, fmt.Errorf("apply zone %s: %w", zone, err)
		}
		if applied {
			res.Zones = append(res.Zones, zone)
			w.publishReload(zone)
		}
		res.Applied += len(byZone[zone])
	}
	return res, nil
}

// publishReload fans out a per-zone reload so northstar serves the change
// immediately instead of waiting for the S3 poll. Best-effort: the poll is the
// backstop, so a publish failure only delays propagation.
func (w *Writer) publishReload(zone string) {
	if w.nc == nil {
		return
	}
	payload, err := json.Marshal(ZoneReload{Zone: zone})
	if err != nil {
		return
	}
	if err := w.nc.Publish(SubjectZoneReload, payload); err != nil {
		slog.Warn("dns writer: publish zone reload", "zone", zone, "error", err)
	}
}

// applyZone read-modify-writes a single zone TOML for its changes. It returns
// whether the zone object was rewritten.
func (w *Writer) applyZone(zone string, changes []Change) (bool, error) {
	cfg, exists, err := nsconfig.ReadZoneRaw(w.s3cfg, zone)
	if err != nil {
		return false, err
	}
	if !exists {
		// Deletes against a missing zone are a no-op; only materialise a zone
		// when there is something to add.
		if !hasUpsert(changes) {
			return false, nil
		}
		cfg = nsconfig.NewZoneConfig(nsconfig.BaseZoneSeed{
			Domain:      zone,
			Nameservers: []nsconfig.NameserverSeed{w.localNS},
		})
	}

	changed := false
	for _, c := range changes {
		label := relativeLabel(c.Name, zone)
		rtype := recordType(c.Type)
		ttl := c.TTL
		if ttl == 0 {
			ttl = w.ttl
		}
		switch c.Action {
		case ActionUpsert:
			if cfg.UpsertRecord(label, rtype, nsconfig.ClassIN, c.Value, ttl) {
				changed = true
			}
		case ActionDelete:
			if cfg.RemoveRecord(label, rtype, c.Value) {
				changed = true
			}
		default:
			return false, fmt.Errorf("unknown action %q", c.Action)
		}
	}
	if !changed {
		return false, nil
	}

	cfg.Domain.Modified = time.Now().UTC()
	body, err := nsconfig.RenderZone(cfg)
	if err != nil {
		return false, err
	}
	if err := nsconfig.WriteZoneFile(w.s3cfg, zone, body); err != nil {
		return false, err
	}
	slog.Info("dns writer: zone updated", "zone", zone, "changes", len(changes))
	return true, nil
}

// recordType maps a textual record type to its DNS numeric type. V1 handles A;
// unknown types default to A.
func recordType(t string) uint16 {
	switch strings.ToUpper(t) {
	case "NS":
		return nsconfig.TypeNS
	case "TXT":
		return nsconfig.TypeTXT
	default:
		return nsconfig.TypeA
	}
}

func hasUpsert(changes []Change) bool {
	for _, c := range changes {
		if c.Action == ActionUpsert {
			return true
		}
	}
	return false
}

// zoneS3Config builds an S3Config for the northstar bucket using the node's
// northstar.toml endpoint/bucket but the system predastore (read-write)
// credentials. ok is false when northstar S3 or credentials are not configured.
func zoneS3Config(cfg *config.Config) (s3cfg *nsconfig.S3Config, baseDomain string, ok bool) {
	if cfg == nil {
		return nil, "", false
	}
	serverCfg, ok := loadNorthstar(cfg)
	if !ok || serverCfg.S3.Bucket == "" || strings.TrimSpace(serverCfg.DefaultDomain) == "" {
		return nil, "", false
	}
	creds := cfg.Predastore
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return nil, "", false
	}
	return &nsconfig.S3Config{
		Endpoint:  serverCfg.S3.Endpoint,
		Region:    serverCfg.S3.Region,
		Bucket:    serverCfg.S3.Bucket,
		AccessKey: creds.AccessKey,
		SecretKey: creds.SecretKey,
		Insecure:  serverCfg.S3.Insecure,
	}, strings.TrimSpace(serverCfg.DefaultDomain), true
}

// ResolveBaseDomain returns the northstar default_domain for producers building
// record names, or "" when DNS registration is not configured. Prefers the
// non-secret cluster-config value so confined services (e.g. vpcd) need not read
// the credential-bearing northstar.toml; falls back to it when absent.
func ResolveBaseDomain(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if d := strings.TrimSpace(cfg.Northstar.DefaultDomain); d != "" {
		return d
	}
	serverCfg, ok := loadNorthstar(cfg)
	if !ok {
		return ""
	}
	return strings.TrimSpace(serverCfg.DefaultDomain)
}

// ResolveInternalDomain returns the northstar internal_domain (AWS-parity private
// zone) for producers building private record names, or "" when DNS registration
// is not configured. Callers fall back to PrivateZone for an empty result.
// Prefers the non-secret cluster-config value, falling back to northstar.toml.
func ResolveInternalDomain(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if d := strings.TrimSpace(cfg.Northstar.InternalDomain); d != "" {
		return d
	}
	serverCfg, ok := loadNorthstar(cfg)
	if !ok {
		return ""
	}
	return strings.TrimSpace(serverCfg.InternalDomain)
}

// loadNorthstar loads the node's northstar.toml; ok is false when no path is set
// or the file cannot be read.
func loadNorthstar(cfg *config.Config) (nsconfig.ServerConfig, bool) {
	path := cfg.Northstar.ConfigPath
	if path == "" {
		return nsconfig.ServerConfig{}, false
	}
	serverCfg, err := nsconfig.LoadServerConfig(path)
	if err != nil {
		slog.Warn("dns: load northstar config", "path", path, "error", err)
		return nsconfig.ServerConfig{}, false
	}
	return serverCfg, true
}

// localNameserverIP returns a reachable address for the local node, used only as
// a fallback nameserver when materialising a missing zone.
func localNameserverIP(cfg *config.Config) string {
	ip := strings.TrimSpace(cfg.AdvertiseIP)
	if ip == "" {
		ip = strings.TrimSpace(cfg.Host)
	}
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if ip == "" || ip == "0.0.0.0" {
		ip = "127.0.0.1"
	}
	return ip
}
