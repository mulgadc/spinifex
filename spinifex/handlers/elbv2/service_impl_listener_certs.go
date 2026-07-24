package handlers_elbv2

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// listenerCertsToSDK renders stored listener certificates as SDK certificates.
func listenerCertsToSDK(certs []ListenerCertificate) []*elbv2.Certificate {
	out := make([]*elbv2.Certificate, 0, len(certs))
	for _, c := range certs {
		out = append(out, &elbv2.Certificate{
			CertificateArn: aws.String(c.CertificateArn),
			IsDefault:      aws.Bool(c.IsDefault),
		})
	}
	return out
}

// AddListenerCertificates attaches additional (SNI) certificates to a secure listener.
// Added certificates are non-default; re-adding an existing certificate is a no-op.
func (s *ELBv2ServiceImpl) AddListenerCertificates(ctx context.Context, input *elbv2.AddListenerCertificatesInput, accountID string) (*elbv2.AddListenerCertificatesOutput, error) {
	if input == nil || input.ListenerArn == nil || *input.ListenerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Certificates) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	listener, err := s.store.GetListenerByArn(*input.ListenerArn)
	if err != nil {
		slog.ErrorContext(ctx, "AddListenerCertificates: failed to get listener", "arn", *input.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if listener == nil || listener.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2ListenerNotFound)
	}
	if !protocolRequiresCert(listener.Protocol) {
		return nil, errors.New(awserrors.ErrorELBv2InvalidConfigurationRequest)
	}

	updated := *listener
	certs := append([]ListenerCertificate(nil), updated.Certificates...)
	seen := make(map[string]bool, len(certs))
	for _, c := range certs {
		seen[c.CertificateArn] = true
	}
	for _, c := range input.Certificates {
		if c == nil || c.CertificateArn == nil || *c.CertificateArn == "" {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		if seen[*c.CertificateArn] {
			continue
		}
		certs = append(certs, ListenerCertificate{CertificateArn: *c.CertificateArn, IsDefault: false})
		seen[*c.CertificateArn] = true
	}
	updated.Certificates = certs

	if err := s.validateListenerCerts(ctx, certs, accountID); err != nil {
		return nil, err
	}

	if err := s.store.PutListener(&updated); err != nil {
		slog.ErrorContext(ctx, "AddListenerCertificates: failed to persist record", "listenerId", updated.ListenerID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	return &elbv2.AddListenerCertificatesOutput{
		Certificates: listenerCertsToSDK(certs),
	}, nil
}

// RemoveListenerCertificates detaches certificates from a listener by ARN. The
// default certificate cannot be removed. Removing an absent certificate is a
// no-op (idempotent).
func (s *ELBv2ServiceImpl) RemoveListenerCertificates(ctx context.Context, input *elbv2.RemoveListenerCertificatesInput, accountID string) (*elbv2.RemoveListenerCertificatesOutput, error) {
	if input == nil || input.ListenerArn == nil || *input.ListenerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Certificates) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	listener, err := s.store.GetListenerByArn(*input.ListenerArn)
	if err != nil {
		slog.ErrorContext(ctx, "RemoveListenerCertificates: failed to get listener", "arn", *input.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if listener == nil || listener.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2ListenerNotFound)
	}

	remove := make(map[string]bool, len(input.Certificates))
	for _, c := range input.Certificates {
		if c == nil || c.CertificateArn == nil || *c.CertificateArn == "" {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		remove[*c.CertificateArn] = true
	}

	updated := *listener
	kept := make([]ListenerCertificate, 0, len(updated.Certificates))
	for _, c := range updated.Certificates {
		if remove[c.CertificateArn] {
			if c.IsDefault {
				// The default certificate is managed via the listener itself.
				return nil, errors.New(awserrors.ErrorELBv2InvalidConfigurationRequest)
			}
			continue
		}
		kept = append(kept, c)
	}
	updated.Certificates = kept

	if err := s.store.PutListener(&updated); err != nil {
		slog.ErrorContext(ctx, "RemoveListenerCertificates: failed to persist record", "listenerId", updated.ListenerID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	return &elbv2.RemoveListenerCertificatesOutput{}, nil
}

// DescribeListenerCertificates returns the certificates attached to a listener.
func (s *ELBv2ServiceImpl) DescribeListenerCertificates(ctx context.Context, input *elbv2.DescribeListenerCertificatesInput, accountID string) (*elbv2.DescribeListenerCertificatesOutput, error) {
	if input == nil || input.ListenerArn == nil || *input.ListenerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	listener, err := s.store.GetListenerByArn(*input.ListenerArn)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeListenerCertificates: failed to get listener", "arn", *input.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if listener == nil || listener.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2ListenerNotFound)
	}

	return &elbv2.DescribeListenerCertificatesOutput{
		Certificates: listenerCertsToSDK(listener.Certificates),
	}, nil
}

// DescribeSSLPolicies returns the fixed catalog of supported security policies.
// An explicit Names filter selects a subset; an unknown name is rejected.
func (s *ELBv2ServiceImpl) DescribeSSLPolicies(ctx context.Context, input *elbv2.DescribeSSLPoliciesInput, _ string) (*elbv2.DescribeSSLPoliciesOutput, error) {
	var names []string
	if input != nil && len(input.Names) > 0 {
		for _, n := range input.Names {
			if n == nil || *n == "" {
				return nil, errors.New(awserrors.ErrorInvalidParameterValue)
			}
			if !isKnownSslPolicy(*n) {
				return nil, errors.New(awserrors.ErrorELBv2SSLPolicyNotFound)
			}
			names = append(names, *n)
		}
	} else {
		names = sslPolicyOrder
	}

	policies := make([]*elbv2.SslPolicy, 0, len(names))
	for _, n := range names {
		policies = append(policies, sslPolicyToSDK(sslPolicyCatalog[n]))
	}

	return &elbv2.DescribeSSLPoliciesOutput{SslPolicies: policies}, nil
}
