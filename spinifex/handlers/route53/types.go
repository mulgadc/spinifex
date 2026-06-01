// Package handlers_route53 owns the AWS Route53 contract surface on
// Spinifex: hosted-zone CRUD, resource-record-set CRUD, and ChangeInfo
// lookups. Sprint 1a scaffolds the interface + stub bodies; real bodies
// land in Sprints 1b/1c per docs/development/feature/route53-v0.md.
package handlers_route53

import "time"

// Hosted-zone-ID prefix matching AWS shape: "Z" + 21 base32 chars.
const HostedZoneIDPrefix = "Z"

// ChangeID prefix matching AWS shape: "C" + 14 hex chars.
const ChangeIDPrefix = "C"

// PredastoreBucket is the predastore bucket name that holds all zone TOML
// files plus sidecar metadata + change records. Created at admin init.
const PredastoreBucket = "dns-zones"

// MetadataPrefix is the sub-path inside PredastoreBucket that holds
// Route53 zone metadata (name, vpcID, comment, change history) as JSON.
// Filenames: {hostedZoneID}.json.
const MetadataPrefix = "_metadata/"

// ChangesPrefix is the sub-path inside PredastoreBucket that holds
// ChangeInfo records keyed by changeID. Filenames: {changeID}.json.
const ChangesPrefix = "_changes/"

// ChangeStatus is the GetChange status value: PENDING until Eclipso
// confirms reload of the zone version the change produced; INSYNC after.
type ChangeStatus string

const (
	ChangeStatusPending ChangeStatus = "PENDING"
	ChangeStatusInSync  ChangeStatus = "INSYNC"
)

// ZoneRecord holds the persisted Route53 zone metadata stored at
// {PredastoreBucket}/{MetadataPrefix}{hostedZoneID}.json.
type ZoneRecord struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Comment         string    `json:"comment,omitempty"`
	PrivateZone     bool      `json:"private_zone"`
	VPCID           string    `json:"vpc_id,omitempty"`
	VPCRegion       string    `json:"vpc_region,omitempty"`
	CallerReference string    `json:"caller_reference"`
	CreatedAt       time.Time `json:"created_at"`
	RecordCount     int64     `json:"record_count"`
}

// ChangeRecord holds the persisted ChangeInfo state stored at
// {PredastoreBucket}/{ChangesPrefix}{changeID}.json. Status starts as
// PENDING and flips to INSYNC after Eclipso publishes
// dns.zone.loaded.{zoneID} carrying a version >= ZoneVersion.
type ChangeRecord struct {
	ID          string       `json:"id"`
	ZoneID      string       `json:"zone_id"`
	Status      ChangeStatus `json:"status"`
	SubmittedAt time.Time    `json:"submitted_at"`
	ZoneVersion uint64       `json:"zone_version"`
	Comment     string       `json:"comment,omitempty"`
}
