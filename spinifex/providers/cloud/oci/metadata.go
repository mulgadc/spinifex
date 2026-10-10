package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// IMDSBaseURL is the OCI instance metadata service. v2 requires the bearer
// header below; v1 is unauthenticated and disabled on current images.
const IMDSBaseURL = "http://169.254.169.254/opc/v2"

// imdsTimeout bounds a metadata read. The service is link-local and answers in
// single-digit milliseconds, so a slow read means it is not there — which is
// the answer a non-OCI host needs, and it should not take the startup path
// down with it.
const imdsTimeout = 5 * time.Second

// VNICMetadata is one entry of /opc/v2/vnics/. Only the fields that identify
// the VNIC are decoded; the rest of the document is not our business.
type VNICMetadata struct {
	VNICID          string `json:"vnicId"`
	MACAddr         string `json:"macAddr"`
	PrivateIP       string `json:"privateIp"`
	SubnetCIDRBlock string `json:"subnetCidrBlock"`
	VirtualRouterIP string `json:"virtualRouterIp"`
	VLANTag         int    `json:"vlanTag"`
}

// InstanceMetadata is the subset of /opc/v2/instance/ the allocator needs.
type InstanceMetadata struct {
	ID                  string `json:"id"`
	CompartmentID       string `json:"compartmentId"`
	Region              string `json:"region"`
	CanonicalRegionName string `json:"canonicalRegionName"`
	AvailabilityDomain  string `json:"availabilityDomain"`
	Shape               string `json:"shape"`
}

// MetadataClient reads the instance metadata service.
type MetadataClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewMetadataClient returns a client for the link-local metadata service.
func NewMetadataClient() *MetadataClient {
	return &MetadataClient{
		BaseURL: IMDSBaseURL,
		HTTP:    &http.Client{Timeout: imdsTimeout},
	}
}

// VNICs returns the VNICs attached to this instance, in Oracle's order.
func (c *MetadataClient) VNICs(ctx context.Context) ([]VNICMetadata, error) {
	var out []VNICMetadata
	if err := c.get(ctx, "/vnics/", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Instance returns this instance's own metadata document.
func (c *MetadataClient) Instance(ctx context.Context) (InstanceMetadata, error) {
	var out InstanceMetadata
	if err := c.get(ctx, "/instance/", &out); err != nil {
		return InstanceMetadata{}, err
	}
	return out, nil
}

func (c *MetadataClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("oci imds %s: %w", path, err)
	}
	// v2 refuses without this exact header; it is a confused-deputy guard, not
	// a credential, and Oracle documents the literal string.
	req.Header.Set("Authorization", "Bearer Oracle")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("oci imds %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oci imds %s: http %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("oci imds %s: decode: %w", path, err)
	}
	return nil
}

// ResolveVNICByInterface maps a host interface name to the VNIC OCID carrying
// its MAC. Matching on MAC rather than on Oracle's VNIC order is what makes
// this safe: the order is not a documented contract, and on the reference host
// the external VNIC is the second entry only by chance.
//
// A MAC-cloned Linux bridge resolves to the same VNIC as the NIC inside it,
// which is the intended answer — on OCI the external datapath is a bridge that
// clones the VNIC's MAC, so an operator naming either gets the same VNIC.
func ResolveVNICByInterface(ctx context.Context, md *MetadataClient, iface string) (VNICMetadata, error) {
	link, err := net.InterfaceByName(iface)
	if err != nil {
		return VNICMetadata{}, fmt.Errorf("oci resolve vnic: interface %q: %w", iface, err)
	}
	mac := normalizeMAC(link.HardwareAddr.String())
	if mac == "" {
		return VNICMetadata{}, fmt.Errorf("oci resolve vnic: interface %q has no MAC", iface)
	}

	vnics, err := md.VNICs(ctx)
	if err != nil {
		return VNICMetadata{}, err
	}
	return matchVNICByMAC(vnics, mac, iface)
}

// matchVNICByMAC is the matching half of ResolveVNICByInterface, split out so
// it can be tested without depending on the host having an OCI NIC.
func matchVNICByMAC(vnics []VNICMetadata, mac, iface string) (VNICMetadata, error) {
	for _, v := range vnics {
		// IMDS reports the MAC uppercase and the kernel reports it lowercase,
		// so a byte comparison of the two strings never matches.
		if normalizeMAC(v.MACAddr) == normalizeMAC(mac) {
			return v, nil
		}
	}
	attached := make([]string, 0, len(vnics))
	for _, v := range vnics {
		attached = append(attached, v.MACAddr)
	}
	return VNICMetadata{}, fmt.Errorf("oci resolve vnic: no VNIC has interface %q's MAC %s (attached: %s)",
		iface, mac, strings.Join(attached, ", "))
}

func normalizeMAC(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
