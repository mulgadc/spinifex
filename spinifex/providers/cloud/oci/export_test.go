package oci

import (
	"fmt"
	"net/http"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
)

// The MAC-matching half of VNIC resolution is deliberately unexported — it is
// an implementation detail of ResolveVNICByInterface — but it is also the part
// worth testing directly, because the alternative is a test that depends on the
// host having an OCI NIC. Exported here for the external test package.
var (
	NormalizeMAC   = normalizeMAC
	MatchVNICByMAC = matchVNICByMAC
)

// The error classifier and the SDK-to-our-types converters carry the logic in
// client.go that is worth testing; the rest of that file is SDK plumbing that
// only a live endpoint exercises.
var (
	Wrap        = wrap
	ToPrivateIP = toPrivateIP
	ToPublicIP  = toPublicIP
	Deref       = deref
)

// NewTestClient points the real SDK-backed client at a local endpoint, so the
// request shapes this package builds — pagination, and the explicit empty
// string that DetachPublicIP depends on — are testable without a tenancy.
func NewTestClient(provider common.ConfigurationProvider, endpoint string, hc *http.Client) (Client, error) {
	c, err := core.NewVirtualNetworkClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	c.Host = endpoint
	c.HTTPClient = hc
	return &apiClient{net: c}, nil
}

// NewTestBlockClient points the real SDK-backed BlockClient at a local
// endpoint, for both the block storage and the compute half.
func NewTestBlockClient(provider common.ConfigurationProvider, endpoint string, hc *http.Client) (BlockClient, error) {
	client, err := NewBlockClientWithProvider(provider)
	if err != nil {
		return nil, err
	}
	c, ok := client.(*blockAPIClient)
	if !ok {
		return nil, fmt.Errorf("NewBlockClientWithProvider built a %T", client)
	}
	c.storage.Host, c.storage.HTTPClient = endpoint, hc
	c.compute.Host, c.compute.HTTPClient = endpoint, hc
	return c, nil
}
