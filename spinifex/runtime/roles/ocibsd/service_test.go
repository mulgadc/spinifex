package ocibsd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	ebsocibsd "github.com/mulgadc/spinifex/spinifex/providers/ebs/ocibsd"
)

const (
	testCompartment = "ocid1.compartment.oc1..test"
	testAD          = "Uocm:US-ASHBURN-AD-1"
	testInstance    = "ocid1.instance.oc1..node-1"
	testClusterID   = "5f0c3a9e1b7d42c68e2a4f1d9b3c7e05"
)

func TestBlockClientForSelectsTheCredential(t *testing.T) {
	cases := []struct {
		auth, file, profile string
		want                string
		wantErr             bool
	}{
		{auth: "", file: "/etc/oci/config", profile: "SPX", want: "config_file /etc/oci/config SPX"},
		{auth: AuthConfigFile, want: "config_file  "},
		{auth: AuthInstancePrincipal, want: "instance_principal"},
		{auth: "api_key", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.auth, func(t *testing.T) {
			var built string
			oldIP, oldCF := newInstancePrincipalBlockClient, newConfigFileBlockClient
			t.Cleanup(func() { newInstancePrincipalBlockClient, newConfigFileBlockClient = oldIP, oldCF })
			newInstancePrincipalBlockClient = func() (oci.BlockClient, error) { built = "instance_principal"; return oci.NewBlockFake(), nil }
			newConfigFileBlockClient = func(path, profile string) (oci.BlockClient, error) {
				built = "config_file " + path + " " + profile
				return oci.NewBlockFake(), nil
			}

			_, err := BlockClientFor(&Config{Auth: tc.auth, ConfigFile: tc.file, ConfigProfile: tc.profile})
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, built)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, built)
		})
	}
}

type harness struct {
	cfg       *Config
	fake      *oci.BlockFake
	initiator *ebsocibsd.FakeInitiator
	md        oci.InstanceMetadata
	mdErr     error
	clientErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{
		cfg: &Config{
			NodeName: "node-1", AvailabilityZone: "ap-southeast-2a",
			ClusterID: testClusterID, CompartmentID: testCompartment, AvailabilityDomain: testAD,
		},
		fake:      oci.NewBlockFake(),
		initiator: ebsocibsd.NewFakeInitiator(),
		md:        oci.InstanceMetadata{ID: testInstance, AvailabilityDomain: testAD},
	}
}

func (h *harness) deps() deps {
	return deps{
		instance:  func(context.Context) (oci.InstanceMetadata, error) { return h.md, h.mdErr },
		newClient: func(*Config) (oci.BlockClient, error) { return h.fake, h.clientErr },
		initiator: h.initiator,
	}
}

func TestNewProviderRefusals(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(h *harness)
		want    string
	}{
		{"a node in another AD", func(h *harness) { h.md.AvailabilityDomain = "Uocm:US-ASHBURN-AD-2" }, `availability domain "Uocm:US-ASHBURN-AD-2"`},
		{"a host that is not an OCI instance", func(h *harness) { h.mdErr = errors.New("dial 169.254.169.254: no route") }, "instance metadata"},
		{"a credential that cannot be loaded", func(h *harness) { h.clientErr = errors.New("no such profile") }, "build the OCI block client"},
		{"a credential without its grants", func(h *harness) { h.fake.FailOp("ListVolumes", errors.New("404 NotAuthorizedOrNotFound")) }, "manage volume-family"},
		{"no cluster ID", func(h *harness) { h.cfg.ClusterID = "" }, "cluster_id is unset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)
			_, err := newProvider(context.Background(), h.cfg, h.deps())
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestNewProviderLogsOutStaleSessionsOfThisCluster(t *testing.T) {
	h := newHarness(t)
	seed := func(clusterID, iqn string) ebsocibsd.Target {
		v, err := h.fake.CreateVolume(context.Background(), oci.CreateVolumeInput{
			CompartmentID: testCompartment, AvailabilityDomain: testAD, SizeGB: oci.MinVolumeSizeGB,
			FreeformTags: map[string]string{ebsocibsd.TagCluster: clusterID},
		})
		require.NoError(t, err)
		target := ebsocibsd.Target{IQN: ebsocibsd.OCIIQNPrefix + iqn, IPv4: "169.254.2.2", Port: 3260}
		h.fake.SeedAttachment(oci.VolumeAttachment{ID: "att-" + iqn, InstanceID: testInstance, VolumeID: v.ID, State: oci.AttachmentDetached, IQN: target.IQN, IPv4: target.IPv4, Port: target.Port})
		h.initiator.SeedSession(target)
		return target
	}
	stale := seed(testClusterID, "stale")
	seed("0123456789abcdef0123456789abcdef", "other-cluster")

	_, err := newProvider(context.Background(), h.cfg, h.deps())
	require.NoError(t, err)
	assert.Equal(t, []string{"logout " + stale.IQN}, h.initiator.Events(), "only the configured cluster_id's sessions are logged out")
}

func TestServeAnswersOnThisNodesSubjects(t *testing.T) {
	h := newHarness(t)
	_, client := testutil.StartTestNATS(t)
	stop, err := serve(context.Background(), client, h.cfg, h.deps())
	require.NoError(t, err)
	t.Cleanup(stop)

	body, err := json.Marshal(ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewLatestVersioned()})
	require.NoError(t, err)
	msg, err := client.Request(ebsprovider.CapabilitiesSubject, body, 5*time.Second)
	require.NoError(t, err)
	var caps ebsprovider.GetCapabilitiesResponse
	require.NoError(t, json.Unmarshal(msg.Data, &caps))
	require.Nil(t, caps.Error)
	assert.Equal(t, ebsocibsd.Capabilities(), caps.Capabilities)

	subject, err := ebsprovider.PublishSubject(h.cfg.NodeName)
	require.NoError(t, err)
	body, err = json.Marshal(ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewLatestVersioned(), VolumeID: "vol-absent", NodeID: h.cfg.NodeName})
	require.NoError(t, err)
	msg, err = client.Request(subject, body, 5*time.Second)
	require.NoError(t, err)
	var published ebsprovider.PublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &published))
	require.NotNil(t, published.Error)
	assert.Equal(t, ebsprovider.ErrorCodeNotFound, published.Error.Code, "the provider is started and answers this node's subject")
}

func TestNewRejectsAnotherConfigType(t *testing.T) {
	_, err := New(struct{}{})
	require.Error(t, err)
}
