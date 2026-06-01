package handlers_route53

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// TestRoute53ServiceImpl_StubsReturnNotImplemented locks Sprint 1a's
// stub contract: every method on Route53ServiceImpl returns
// awserrors.ErrorNotImplemented. Sprint 1b/1c flips these cases out as
// real bodies land — one method moves out of this list per sprint
// instead of breaking the suite wholesale.
func TestRoute53ServiceImpl_StubsReturnNotImplemented(t *testing.T) {
	svc := NewRoute53ServiceImplWithNATS(nil)

	cases := []struct {
		name string
		call func() error
	}{
		{"CreateHostedZone", func() error {
			_, err := svc.CreateHostedZone(&route53.CreateHostedZoneInput{}, "acct")
			return err
		}},
		{"GetHostedZone", func() error {
			_, err := svc.GetHostedZone(&route53.GetHostedZoneInput{}, "acct")
			return err
		}},
		{"ListHostedZones", func() error {
			_, err := svc.ListHostedZones(&route53.ListHostedZonesInput{}, "acct")
			return err
		}},
		{"UpdateHostedZoneComment", func() error {
			_, err := svc.UpdateHostedZoneComment(&route53.UpdateHostedZoneCommentInput{}, "acct")
			return err
		}},
		{"DeleteHostedZone", func() error {
			_, err := svc.DeleteHostedZone(&route53.DeleteHostedZoneInput{}, "acct")
			return err
		}},
		{"ChangeResourceRecordSets", func() error {
			_, err := svc.ChangeResourceRecordSets(&route53.ChangeResourceRecordSetsInput{}, "acct")
			return err
		}},
		{"ListResourceRecordSets", func() error {
			_, err := svc.ListResourceRecordSets(&route53.ListResourceRecordSetsInput{}, "acct")
			return err
		}},
		{"GetChange", func() error {
			_, err := svc.GetChange(&route53.GetChangeInput{}, "acct")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("got nil, want NotImplemented")
			}
			if !errors.Is(err, errors.New(awserrors.ErrorNotImplemented)) && err.Error() != awserrors.ErrorNotImplemented {
				t.Errorf("err = %q, want %q", err.Error(), awserrors.ErrorNotImplemented)
			}
		})
	}
}
