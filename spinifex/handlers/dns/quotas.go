package dns

// Route 53 and Route 53 Resolver service quotas, mirrored from AWS defaults:
// https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/DNSLimitations.html
//
// The consts are the AWS default values. Soft entity quotas (increasable on
// request at AWS) are also surfaced on Quotas so a future [quotas] config block
// can override them per deployment; the API-request maxima are fixed by the
// wire contract and stay consts.
const (
	// DefaultHostedZonesPerAccount is AWS's initial per-account hosted-zone cap.
	DefaultHostedZonesPerAccount = 500

	// DefaultRecordsPerHostedZone is AWS's per-hosted-zone record cap. AWS bills
	// beyond it; we reject a new record set past it (see Writer.applyZone).
	DefaultRecordsPerHostedZone = 10_000

	// DefaultVPCsPerPrivateZone is AWS's cap on VPC associations per private zone.
	DefaultVPCsPerPrivateZone = 300

	// DefaultReusableDelegationSetsPerAccount is AWS's per-account cap.
	DefaultReusableDelegationSetsPerAccount = 100

	// DefaultResolverEndpointsPerRegion / DefaultResolverIPsPerEndpoint /
	// DefaultResolverRulesPerRegion are the Route 53 Resolver entity caps.
	DefaultResolverEndpointsPerRegion = 4
	DefaultResolverIPsPerEndpoint     = 6
	DefaultResolverRulesPerRegion     = 1_000

	// DefaultResolverQPSPerIP is AWS's 10,000 UDP queries/sec per resolver-endpoint
	// IP. The per-tap 169.254.169.253 shim is our resolver-endpoint analog, so
	// this is the AWS-parity ceiling above which the forwarder sheds a flood
	// (handlers/imds dnsQueryRatePerTap mirrors it).
	DefaultResolverQPSPerIP = 10_000

	// MaxRecordsPerChangeRequest is the hard cap on ResourceRecord elements in one
	// ChangeResourceRecordSets request (an UPSERT counts each element twice).
	MaxRecordsPerChangeRequest = 1_000

	// MaxValueCharsPerChangeRequest is the hard cap on the summed characters of all
	// Value elements in one change request (an UPSERT counts each character twice).
	MaxValueCharsPerChangeRequest = 32_000

	// MaxValuesPerRecordSet is the hard cap on values (records) in one record set.
	MaxValuesPerRecordSet = 400

	// MaxSameNameTypeRoutedRecords is the cap on weighted/latency/geolocation/
	// multivalue/IP-based records sharing one name and type.
	MaxSameNameTypeRoutedRecords = 100

	// Route53APIRequestsPerSecond is AWS's per-account API request rate.
	Route53APIRequestsPerSecond = 5
)

// Quotas holds the soft, per-deployment-overridable service quotas. A future
// config file ([quotas] in northstar.toml or the spinifex node config) can
// populate this; DefaultQuotas mirrors the AWS defaults above. The fixed
// API-request maxima are not included — they are wire-contract consts.
type Quotas struct {
	HostedZonesPerAccount int
	RecordsPerHostedZone  int
	VPCsPerPrivateZone    int
	ResolverQPSPerIP      int
}

// DefaultQuotas returns the AWS-default service quotas.
func DefaultQuotas() Quotas {
	return Quotas{
		HostedZonesPerAccount: DefaultHostedZonesPerAccount,
		RecordsPerHostedZone:  DefaultRecordsPerHostedZone,
		VPCsPerPrivateZone:    DefaultVPCsPerPrivateZone,
		ResolverQPSPerIP:      DefaultResolverQPSPerIP,
	}
}

// withinRecordQuota reports whether a hosted zone currently holding `current`
// record sets may accept one more. Equal-or-over the cap rejects the add.
func (q Quotas) withinRecordQuota(current int) bool {
	return current < q.RecordsPerHostedZone
}
