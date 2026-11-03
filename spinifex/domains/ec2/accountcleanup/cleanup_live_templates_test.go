package accountcleanup

//test:in-package — shares the owner fixtures and the injected clock.

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConsumers answers what nodegroups name; a non-nil err is returned with
// whatever partial answer it also holds.
type fakeConsumers struct {
	ids, names []string
	err        error
}

func (f *fakeConsumers) AccountTemplateConsumers(context.Context, string) (*TemplateConsumers, error) {
	out := &TemplateConsumers{TemplateIDs: map[string]bool{}, TemplateNames: map[string]bool{}}
	markAll(out.TemplateIDs, f.ids)
	markAll(out.TemplateNames, f.names)
	return out, f.err
}

var _ TemplateConsumerReader = (*fakeConsumers)(nil)

// liveDelete runs a cleanup that deletes live templates, with consumers as the
// nodegroup reader (nil means none supplied).
func (o *owners) liveDelete(t *testing.T, consumers TemplateConsumerReader) *Result {
	t.Helper()
	readers := o.readers()
	readers.Consumers = consumers
	c := New(o.keys, o.groups, o.templates, o.tags, readers)
	c.now = func() time.Time { return time.Now().Add(time.Hour) }
	res, err := c.CleanupPlatform(t.Context(), &Request{AccountID: testAccount, CorrelationRef: "ref-1", DeleteLiveTemplates: true})
	require.NoError(t, err)
	return res
}

func (o *owners) templateNamingKeyAndGroup(t *testing.T, name, key, group string) string {
	t.Helper()
	ltID := o.createTemplate(t, testAccount, name)
	_, err := o.templates.CreateLaunchTemplateVersion(t.Context(), &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: aws.String(ltID),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			KeyName:   aws.String(key),
			Placement: &ec2.LaunchTemplatePlacementRequest{GroupName: aws.String(group)},
		},
	}, testAccount)
	require.NoError(t, err)
	return ltID
}

func (o *owners) liveTemplateIDs(t *testing.T) []string {
	t.Helper()
	scan, err := o.templates.ScanAccount(t.Context(), testAccount)
	require.NoError(t, err)
	return scan.LiveTemplateIDs
}

// A template no nodegroup names is deleted in the same run as the key pair and
// group its version names, which the reference check no longer sees.
func TestCleanupPlatformDeletesAnUnnamedTemplateAndWhatItNames(t *testing.T) {
	o := newOwners(t)
	key := o.importKey(t, testAccount, "tkey")
	group := o.createGroup(t, testAccount, "tgroup")
	lt := o.templateNamingKeyAndGroup(t, "web", "tkey", "tgroup")

	res := o.liveDelete(t, &fakeConsumers{})
	assert.ElementsMatch(t, []Item{
		item(lt, ec2v1.PlatformKindLaunchTemplate, DispositionDeleted, ""),
		item(key, ec2v1.PlatformKindKeyPair, DispositionDeleted, ""),
		item(group, ec2v1.PlatformKindPlacementGroup, DispositionDeleted, ""),
	}, res.Items)
	assert.Empty(t, o.liveTemplateIDs(t))
}

// A template a nodegroup names, by ID or by name, is kept and blocked, and so
// are the key pair and group its versions name.
func TestCleanupPlatformKeepsATemplateANodegroupNames(t *testing.T) {
	for name, consumers := range map[string]*fakeConsumers{
		"by id":   {},
		"by name": {names: []string{"web"}},
	} {
		t.Run(name, func(t *testing.T) {
			o := newOwners(t)
			key := o.importKey(t, testAccount, "tkey")
			group := o.createGroup(t, testAccount, "tgroup")
			lt := o.templateNamingKeyAndGroup(t, "web", "tkey", "tgroup")
			other := o.createTemplate(t, testAccount, "free")
			if name == "by id" {
				consumers.ids = []string{lt}
			}

			res := o.liveDelete(t, consumers)
			assert.ElementsMatch(t, []Item{
				item(lt, ec2v1.PlatformKindLaunchTemplate, DispositionBlocked, ReasonLaunchTemplateReference),
				item(other, ec2v1.PlatformKindLaunchTemplate, DispositionDeleted, ""),
				item(key, ec2v1.PlatformKindKeyPair, DispositionBlocked, ReasonLaunchTemplateReference),
				item(group, ec2v1.PlatformKindPlacementGroup, DispositionBlocked, ReasonLaunchTemplateReference),
			}, res.Items)
			assert.Equal(t, []string{lt}, o.liveTemplateIDs(t))
		})
	}
}

// A consumer read that fails, returns a partial answer with an error, or is not
// supplied deletes no template, key pair or group, and the kind is incomplete.
func TestCleanupPlatformUnreadableConsumersDeleteNoTemplate(t *testing.T) {
	cases := map[string]TemplateConsumerReader{
		"error":        &fakeConsumers{err: errInjected},
		"partial":      &fakeConsumers{ids: []string{"lt-0other"}, err: errInjected},
		"not supplied": nil,
	}
	for name, consumers := range cases {
		t.Run(name, func(t *testing.T) {
			o := newOwners(t)
			key := o.importKey(t, testAccount, "tkey")
			lt := o.templateNamingKeyAndGroup(t, "web", "tkey", "none")

			res := o.liveDelete(t, consumers)
			assert.Contains(t, res.Items, item(lt, ec2v1.PlatformKindLaunchTemplate, DispositionBlocked, ReasonReferenceCheckFailed))
			assert.Contains(t, res.Items, item(key, ec2v1.PlatformKindKeyPair, DispositionBlocked, ReasonLaunchTemplateReference))
			assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, statuses(res)[ec2v1.PlatformKindLaunchTemplate])
			assert.Equal(t, []string{lt}, o.liveTemplateIDs(t))
		})
	}
}

// A template only a DELETING nodegroup still names is as blocked as any other:
// the reader reports every status, so the cleanup never sees a status to trust.
func TestCleanupPlatformKeepsATemplateADeletingNodegroupStillNames(t *testing.T) {
	o := newOwners(t)
	lt := o.createTemplate(t, testAccount, "draining")

	res := o.liveDelete(t, &fakeConsumers{ids: []string{lt}})
	assert.Equal(t, []Item{item(lt, ec2v1.PlatformKindLaunchTemplate, DispositionBlocked, ReasonLaunchTemplateReference)}, res.Items)
	assert.Equal(t, []string{lt}, o.liveTemplateIDs(t))
}

// Without the request flag a supplied consumer reader changes nothing: live
// templates are never deleted, as the exercise relies on.
func TestCleanupPlatformLiveTemplatesSurviveWithoutTheRequestFlag(t *testing.T) {
	o := newOwners(t)
	lt := o.createTemplate(t, testAccount, "web")
	readers := o.readers()
	readers.Consumers = &fakeConsumers{}
	c := New(o.keys, o.groups, o.templates, o.tags, readers)

	res := run(t, c)
	assert.Equal(t, []Item{item(lt, ec2v1.PlatformKindLaunchTemplate, DispositionBlocked, ReasonReferenceUnknown)}, res.Items)
	assert.Equal(t, []string{lt}, o.liveTemplateIDs(t))
}
