package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
)

// AccountQuotaRequest addresses one account. PutAccountQuota also carries the
// overrides; GetAccountQuota ignores them.
type AccountQuotaRequest struct {
	AccountID string                   `json:"accountId"`
	Overrides handlers_quota.Overrides `json:"overrides,omitzero"`
}

// AccountQuotaResponse reports the limits in force for an account and where
// each came from. The source map is what makes a surprising number debuggable:
// a bare limit cannot say whether it was inherited or set.
type AccountQuotaResponse struct {
	AccountID string                   `json:"accountId"`
	Limits    map[string]int           `json:"limits"`
	Source    map[string]string        `json:"source"`
	Overrides handlers_quota.Overrides `json:"overrides"`
}

// sourceConfig and sourceOverride name where a resolved limit came from.
const (
	sourceConfig   = "config"
	sourceOverride = "override"
)

// adminGetAccountQuota reports one account's effective limits, its stored
// overrides, and which layer each dimension resolved from.
func (gw *GatewayConfig) adminGetAccountQuota(ctx context.Context, body []byte) (any, error) {
	accountID, _, err := gw.parseAccountQuotaRequest(body)
	if err != nil {
		return nil, err
	}

	over, limits, err := gw.Quota.GetAccountQuota(ctx, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "GetAccountQuota: failed to read override", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return accountQuotaResponse(accountID, over, limits), nil
}

// adminPutAccountQuota stores one account's overrides and returns the limits
// they resolve to, so a caller never has to guess what it just applied. An
// empty override set clears the record.
func (gw *GatewayConfig) adminPutAccountQuota(ctx context.Context, body []byte) (any, error) {
	accountID, over, err := gw.parseAccountQuotaRequest(body)
	if err != nil {
		return nil, err
	}

	// The caller's own identity, so a quota change names who made it.
	updatedBy, _ := ctx.Value(ctxIdentity).(string)
	if err := gw.Quota.PutAccountQuota(ctx, accountID, over, updatedBy); err != nil {
		slog.ErrorContext(ctx, "PutAccountQuota: failed to store override", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	slog.InfoContext(ctx, "Account quota override updated",
		"accountID", accountID, "updatedBy", updatedBy, "cleared", over.Empty())

	stored, limits, err := gw.Quota.GetAccountQuota(ctx, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "PutAccountQuota: failed to read back override", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return accountQuotaResponse(accountID, stored, limits), nil
}

// parseAccountQuotaRequest decodes and validates a quota request. The account
// must exist: silently accepting an override for a typo'd ID would store a
// record that never applies to anything.
func (gw *GatewayConfig) parseAccountQuotaRequest(body []byte) (string, handlers_quota.Overrides, error) {
	if gw.Quota == nil || gw.IAMService == nil {
		slog.Error("AccountQuota: quota or IAM service not available")
		return "", handlers_quota.Overrides{}, errors.New(awserrors.ErrorInternalError)
	}

	var req AccountQuotaRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", handlers_quota.Overrides{}, errors.New(awserrors.ErrorInvalidRequest)
	}
	if !accountIDRE.MatchString(req.AccountID) {
		return "", handlers_quota.Overrides{}, errors.New(awserrors.ErrorInvalidRequest)
	}
	if _, err := gw.IAMService.GetAccount(req.AccountID); err != nil {
		return "", handlers_quota.Overrides{}, errors.New(awserrors.ErrorInvalidRequest)
	}
	return req.AccountID, req.Overrides, nil
}

// accountQuotaResponse pairs each resolved limit with the layer it came from.
func accountQuotaResponse(accountID string, over handlers_quota.Overrides, limits handlers_quota.Limits) *AccountQuotaResponse {
	dimensions := []struct {
		name     string
		value    int
		override *int
	}{
		{"vcpus", limits.VCPUs, over.VCPUs},
		{"vpcs", limits.VPCs, over.VPCs},
		{"subnets", limits.Subnets, over.Subnets},
		{"eips", limits.EIPs, over.EIPs},
		{"volumes", limits.Volumes, over.Volumes},
		{"volumes_gib", limits.VolumesGiB, over.VolumesGiB},
		{"rds_instances", limits.RDSInstances, over.RDSInstances},
		{"load_balancers", limits.LoadBalancers, over.LoadBalancers},
	}

	resp := &AccountQuotaResponse{
		AccountID: accountID,
		Limits:    make(map[string]int, len(dimensions)),
		Source:    make(map[string]string, len(dimensions)),
		Overrides: over,
	}
	for _, d := range dimensions {
		resp.Limits[d.name] = d.value
		resp.Source[d.name] = sourceConfig
		if d.override != nil {
			resp.Source[d.name] = sourceOverride
		}
	}
	return resp
}
