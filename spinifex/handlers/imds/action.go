package handlers_imds

import (
	"net/http"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
)

// actionOther is the catch-all action name. A guest controls the request path,
// so every unrecognised path must collapse to one value or a crawl of invented
// paths would inflate the metric dimension without bound.
const actionOther = "imds.other"

// actionRejectedForwarded names an SSRF probe turned away by rejectForwarded,
// which answers before nameAction runs.
const actionRejectedForwarded = "imds.rejected.forwarded"

// metaDataActions names the metadata keys dispatch serves, keyed by the path
// suffix after prefixMetaData. Trailing slashes are trimmed before lookup, so
// only the bare form appears here.
var metaDataActions = map[string]string{
	"":                            "imds.meta-data.root",
	"instance-id":                 "imds.meta-data.instance-id",
	"instance-life-cycle":         "imds.meta-data.instance-life-cycle",
	"instance-type":               "imds.meta-data.instance-type",
	"ami-id":                      "imds.meta-data.ami-id",
	"ami-launch-index":            "imds.meta-data.ami-launch-index",
	"reservation-id":              "imds.meta-data.reservation-id",
	"local-ipv4":                  "imds.meta-data.local-ipv4",
	"public-ipv4":                 "imds.meta-data.public-ipv4",
	"public-hostname":             "imds.meta-data.public-hostname",
	"hostname":                    "imds.meta-data.hostname",
	"local-hostname":              "imds.meta-data.local-hostname",
	"mac":                         "imds.meta-data.mac",
	"security-groups":             "imds.meta-data.security-groups",
	"network":                     "imds.meta-data.network",
	"network/interfaces":          "imds.meta-data.network.interfaces",
	"network/interfaces/macs":     "imds.meta-data.network.interfaces.macs",
	"placement":                   "imds.meta-data.placement",
	"placement/availability-zone": "imds.meta-data.placement.availability-zone",
	"placement/region":            "imds.meta-data.placement.region",
	"services":                    "imds.meta-data.services",
	"services/domain":             "imds.meta-data.services.domain",
	"services/partition":          "imds.meta-data.services.partition",
	"iam":                         "imds.meta-data.iam",
	"iam/info":                    "imds.meta-data.iam.info",
}

// imdsAction maps a request to a bounded action name for request metrics. The
// role name, the MAC and the public-key index are stripped: they identify a
// resource and would turn one action into one series per guest.
func imdsAction(method, path string) string {
	if method == http.MethodPut && path == pathToken {
		return "imds.api.token"
	}
	switch path {
	case pathToken:
		return "imds.api.token"
	case pathSpinifexCACert:
		return "imds.spinifex.ca-cert"
	case pathUserData:
		return "imds.user-data"
	case pathIdentityDocument:
		return "imds.dynamic.instance-identity.document"
	case "/":
		return "imds.versions"
	}

	trimmed := strings.TrimSuffix(path, "/")
	switch trimmed {
	case "/latest":
		return "imds.latest"
	case prefixDynamic:
		return "imds.dynamic"
	case pathIdentityDir:
		return "imds.dynamic.instance-identity"
	case pathSecurityCredsDir:
		return "imds.iam.security-credentials.list"
	case pathPublicKeysDir:
		return "imds.public-keys"
	}

	if strings.HasPrefix(path, prefixSecurityCreds) {
		if len(path) > len(prefixSecurityCreds) {
			return "imds.iam.security-credentials.role"
		}
		return "imds.iam.security-credentials.list"
	}
	if strings.HasPrefix(path, prefixNetworkMacs) {
		return "imds.meta-data.network.interfaces.macs"
	}
	if strings.HasPrefix(path, prefixPublicKeys) {
		return "imds.public-keys"
	}
	if sub, ok := strings.CutPrefix(trimmed, pathMetaDataRoot); ok {
		if action, known := metaDataActions[strings.TrimPrefix(sub, "/")]; known {
			return action
		}
	}
	return actionOther
}

// nameAction records the bounded action for the in-flight request so request
// metrics carry the IMDS endpoint rather than the bare HTTP verb. It runs
// inside normalizeVersion, so the path it reads is already canonical /latest.
func nameAction(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otelsetup.SetRequestAction(r.Context(), imdsAction(r.Method, r.URL.Path))
		next.ServeHTTP(w, r)
	})
}
