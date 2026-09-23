package oci

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
)

// Client is the narrow surface the allocator needs. It is expressed in this
// package's own types rather than the SDK's so the SDK stays confined to this
// file and the fake in fake.go is a map rather than a mock framework.
type Client interface {
	// AssignPrivateIP creates a secondary private IP on vnicID. addr may be
	// the zero Addr to let OCI choose from the subnet, which is the normal
	// case — we do not pick addresses, OCI does.
	AssignPrivateIP(ctx context.Context, vnicID string, addr netip.Addr, displayName string) (PrivateIP, error)
	// UnassignPrivateIP deletes a secondary private IP. Deleting a primary is
	// refused by OCI, which is the behaviour we want.
	UnassignPrivateIP(ctx context.Context, privateIPID string) error
	GetPrivateIP(ctx context.Context, privateIPID string) (PrivateIP, error)
	ListPrivateIPs(ctx context.Context, vnicID string) ([]PrivateIP, error)

	// CreatePublicIP creates a RESERVED public IP in compartmentID and, when
	// privateIPID is non-empty, attaches it in the same call.
	CreatePublicIP(ctx context.Context, compartmentID, privateIPID, displayName string) (PublicIP, error)
	// AttachPublicIP moves an existing reserved public IP onto privateIPID.
	AttachPublicIP(ctx context.Context, publicIPID, privateIPID string) (PublicIP, error)
	// DetachPublicIP unassigns without deleting, so the address is kept.
	DetachPublicIP(ctx context.Context, publicIPID string) (PublicIP, error)
	DeletePublicIP(ctx context.Context, publicIPID string) error
	GetPublicIP(ctx context.Context, publicIPID string) (PublicIP, error)
	// GetPublicIPByPrivateIPID returns the public IP attached to a private IP,
	// or ErrNotFound when it has none.
	GetPublicIPByPrivateIPID(ctx context.Context, privateIPID string) (PublicIP, error)
}

// apiClient is the real implementation over the OCI Go SDK.
type apiClient struct {
	net core.VirtualNetworkClient
}

var _ Client = (*apiClient)(nil)

// NewInstancePrincipalClient authenticates as the instance itself, using the
// certificate the metadata service serves at /opc/v2/identity/cert.pem. No key
// material on disk and nothing to rotate, which is why this is the only
// constructor offered for the daemon path — a node that can reach its own
// metadata service is already proving it is the instance it claims to be.
func NewInstancePrincipalClient() (Client, error) {
	provider, err := auth.InstancePrincipalConfigurationProvider()
	if err != nil {
		return nil, fmt.Errorf("oci instance principal: %w", err)
	}
	c, err := core.NewVirtualNetworkClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("oci virtual network client: %w", err)
	}
	return &apiClient{net: c}, nil
}

// NewClientWithProvider builds a client from an explicit configuration
// provider, for an operator running off a config file rather than on an
// instance.
func NewClientWithProvider(p common.ConfigurationProvider) (Client, error) {
	c, err := core.NewVirtualNetworkClientWithConfigurationProvider(p)
	if err != nil {
		return nil, fmt.Errorf("oci virtual network client: %w", err)
	}
	return &apiClient{net: c}, nil
}

func (c *apiClient) AssignPrivateIP(ctx context.Context, vnicID string, addr netip.Addr, displayName string) (PrivateIP, error) {
	details := core.CreatePrivateIpDetails{VnicId: &vnicID}
	if displayName != "" {
		details.DisplayName = &displayName
	}
	if addr.IsValid() {
		s := addr.String()
		details.IpAddress = &s
	}
	resp, err := c.net.CreatePrivateIp(ctx, core.CreatePrivateIpRequest{CreatePrivateIpDetails: details})
	if err != nil {
		return PrivateIP{}, wrap("AssignPrivateIP", err)
	}
	return toPrivateIP(resp.PrivateIp)
}

func (c *apiClient) UnassignPrivateIP(ctx context.Context, privateIPID string) error {
	_, err := c.net.DeletePrivateIp(ctx, core.DeletePrivateIpRequest{PrivateIpId: &privateIPID})
	return wrap("UnassignPrivateIP", err)
}

func (c *apiClient) GetPrivateIP(ctx context.Context, privateIPID string) (PrivateIP, error) {
	resp, err := c.net.GetPrivateIp(ctx, core.GetPrivateIpRequest{PrivateIpId: &privateIPID})
	if err != nil {
		return PrivateIP{}, wrap("GetPrivateIP", err)
	}
	return toPrivateIP(resp.PrivateIp)
}

func (c *apiClient) ListPrivateIPs(ctx context.Context, vnicID string) ([]PrivateIP, error) {
	var out []PrivateIP
	var page *string
	for {
		resp, err := c.net.ListPrivateIps(ctx, core.ListPrivateIpsRequest{VnicId: &vnicID, Page: page})
		if err != nil {
			return nil, wrap("ListPrivateIPs", err)
		}
		for _, item := range resp.Items {
			ip, err := toPrivateIP(item)
			if err != nil {
				return nil, err
			}
			out = append(out, ip)
		}
		if resp.OpcNextPage == nil {
			return out, nil
		}
		page = resp.OpcNextPage
	}
}

func (c *apiClient) CreatePublicIP(ctx context.Context, compartmentID, privateIPID, displayName string) (PublicIP, error) {
	details := core.CreatePublicIpDetails{
		CompartmentId: &compartmentID,
		Lifetime:      core.CreatePublicIpDetailsLifetimeReserved,
	}
	if displayName != "" {
		details.DisplayName = &displayName
	}
	if privateIPID != "" {
		details.PrivateIpId = &privateIPID
	}
	resp, err := c.net.CreatePublicIp(ctx, core.CreatePublicIpRequest{CreatePublicIpDetails: details})
	if err != nil {
		return PublicIP{}, wrap("CreatePublicIP", err)
	}
	return toPublicIP(resp.PublicIp)
}

func (c *apiClient) AttachPublicIP(ctx context.Context, publicIPID, privateIPID string) (PublicIP, error) {
	resp, err := c.net.UpdatePublicIp(ctx, core.UpdatePublicIpRequest{
		PublicIpId:            &publicIPID,
		UpdatePublicIpDetails: core.UpdatePublicIpDetails{PrivateIpId: &privateIPID},
	})
	if err != nil {
		return PublicIP{}, wrap("AttachPublicIP", err)
	}
	return toPublicIP(resp.PublicIp)
}

// DetachPublicIP clears privateIpId. The SDK omits a nil pointer, so the empty
// string is sent explicitly — that is what OCI reads as "unassign", and a nil
// here would be a no-op update that reports success while changing nothing.
func (c *apiClient) DetachPublicIP(ctx context.Context, publicIPID string) (PublicIP, error) {
	empty := ""
	resp, err := c.net.UpdatePublicIp(ctx, core.UpdatePublicIpRequest{
		PublicIpId:            &publicIPID,
		UpdatePublicIpDetails: core.UpdatePublicIpDetails{PrivateIpId: &empty},
	})
	if err != nil {
		return PublicIP{}, wrap("DetachPublicIP", err)
	}
	return toPublicIP(resp.PublicIp)
}

func (c *apiClient) DeletePublicIP(ctx context.Context, publicIPID string) error {
	_, err := c.net.DeletePublicIp(ctx, core.DeletePublicIpRequest{PublicIpId: &publicIPID})
	return wrap("DeletePublicIP", err)
}

func (c *apiClient) GetPublicIP(ctx context.Context, publicIPID string) (PublicIP, error) {
	resp, err := c.net.GetPublicIp(ctx, core.GetPublicIpRequest{PublicIpId: &publicIPID})
	if err != nil {
		return PublicIP{}, wrap("GetPublicIP", err)
	}
	return toPublicIP(resp.PublicIp)
}

func (c *apiClient) GetPublicIPByPrivateIPID(ctx context.Context, privateIPID string) (PublicIP, error) {
	resp, err := c.net.GetPublicIpByPrivateIpId(ctx, core.GetPublicIpByPrivateIpIdRequest{
		GetPublicIpByPrivateIpIdDetails: core.GetPublicIpByPrivateIpIdDetails{PrivateIpId: &privateIPID},
	})
	if err != nil {
		return PublicIP{}, wrap("GetPublicIPByPrivateIPID", err)
	}
	return toPublicIP(resp.PublicIp)
}

func toPrivateIP(p core.PrivateIp) (PrivateIP, error) {
	out := PrivateIP{
		ID:          deref(p.Id),
		VNICID:      deref(p.VnicId),
		SubnetID:    deref(p.SubnetId),
		DisplayName: deref(p.DisplayName),
		IsPrimary:   p.IsPrimary != nil && *p.IsPrimary,
	}
	if s := deref(p.IpAddress); s != "" {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return PrivateIP{}, fmt.Errorf("oci: parse private ip %q: %w", s, err)
		}
		out.Address = addr
	}
	return out, nil
}

func toPublicIP(p core.PublicIp) (PublicIP, error) {
	out := PublicIP{
		ID:             deref(p.Id),
		PrivateIPID:    deref(p.PrivateIpId),
		DisplayName:    deref(p.DisplayName),
		Lifetime:       string(p.Lifetime),
		LifecycleState: string(p.LifecycleState),
	}
	if s := deref(p.IpAddress); s != "" {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return PublicIP{}, fmt.Errorf("oci: parse public ip %q: %w", s, err)
		}
		out.Address = addr
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// wrap classifies an SDK error against the three sentinels. The HTTP status is
// the reliable signal; the service code is carried for the message but not
// matched on, because OCI's code strings differ per service and per endpoint
// while the statuses do not.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	svc, ok := common.IsServiceError(err)
	if !ok {
		return fmt.Errorf("oci %s: %w", op, err)
	}
	api := &APIError{
		Op:         op,
		Code:       svc.GetCode(),
		Message:    svc.GetMessage(),
		StatusCode: svc.GetHTTPStatusCode(),
		RequestID:  svc.GetOpcRequestID(),
	}
	switch {
	case svc.GetHTTPStatusCode() == 404:
		api.kind = ErrNotFound
	case svc.GetHTTPStatusCode() == 409, svc.GetHTTPStatusCode() == 412:
		api.kind = ErrConflict
	case svc.GetHTTPStatusCode() == 400 && svc.GetCode() == "LimitExceeded":
		api.kind = ErrLimitExceeded
	case svc.GetHTTPStatusCode() == 429:
		api.kind = ErrLimitExceeded
	}
	return api
}
