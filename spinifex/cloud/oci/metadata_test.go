package oci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The document shape is taken verbatim from mulga-poc, uppercase MACs and all.
const vnicsDoc = `[
  {"macAddr":"02:00:17:00:EF:07","privateIp":"10.200.0.144","subnetCidrBlock":"10.200.0.0/24",
   "virtualRouterIp":"10.200.0.1","vlanTag":119,
   "vnicId":"ocid1.vnic.oc1.ap-sydney-1.abzxsljrs4pqgaicbsgasbozqrjh3bocqvqpsu5gda2a2aseijmow67t7nsq"},
  {"macAddr":"02:00:17:01:7E:71","privateIp":"10.200.0.29","subnetCidrBlock":"10.200.0.0/24",
   "virtualRouterIp":"10.200.0.1","vlanTag":1603,
   "vnicId":"ocid1.vnic.oc1.ap-sydney-1.abzxsljrygicziuh2jktbre2ftvqxe2yh7lby2rrsqcbu6irus2sg73ajj5a"}
]`

const instanceDoc = `{
  "id":"ocid1.instance.oc1.ap-sydney-1.anzxsljr6eq57gicdlugsqfidppe356dhslyar7pq762qpgkdv2loyrv7uyq",
  "compartmentId":"ocid1.compartment.oc1..aaaaaaaa6nkkqia2q3z2gjmqhqpq66t4k42keqob5wphgvtrfwlkfbr3ztta",
  "region":"ap-sydney-1","canonicalRegionName":"ap-sydney-1",
  "availabilityDomain":"RgrR:AP-SYDNEY-1-AD-1","shape":"VM.Standard.E6.Flex"
}`

func testMetadata(t *testing.T) (*oci.MetadataClient, *[]string) {
	t.Helper()
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/vnics/":
			_, _ = w.Write([]byte(vnicsDoc))
		case "/instance/":
			_, _ = w.Write([]byte(instanceDoc))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &oci.MetadataClient{BaseURL: srv.URL, HTTP: srv.Client()}, &auth
}

func TestVNICsDecodesTheMetadataDocument(t *testing.T) {
	md, auth := testMetadata(t)

	vnics, err := md.VNICs(context.Background())
	require.NoError(t, err)
	require.Len(t, vnics, 2)

	assert.Equal(t, "10.200.0.29", vnics[1].PrivateIP)
	assert.Equal(t, 1603, vnics[1].VLANTag)
	assert.Contains(t, vnics[1].VNICID, "ocid1.vnic.oc1.ap-sydney-1.")
	// IMDS v2 refuses the request without the literal bearer header.
	assert.Equal(t, []string{"Bearer Oracle"}, *auth)
}

func TestInstanceCarriesTheCompartment(t *testing.T) {
	md, _ := testMetadata(t)

	inst, err := md.Instance(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ocid1.compartment.oc1..aaaaaaaa6nkkqia2q3z2gjmqhqpq66t4k42keqob5wphgvtrfwlkfbr3ztta", inst.CompartmentID)
	assert.Equal(t, "ap-sydney-1", inst.CanonicalRegionName)
}

// IMDS reports MACs uppercase and the kernel reports them lowercase, so a byte
// comparison of the two never matches and every resolve would fail.
func TestMACMatchingIsCaseInsensitive(t *testing.T) {
	assert.Equal(t, oci.NormalizeMAC("02:00:17:01:7E:71"), oci.NormalizeMAC("02:00:17:01:7e:71"))
	assert.NotEqual(t, oci.NormalizeMAC("02:00:17:01:7E:71"), oci.NormalizeMAC("02:00:17:00:ef:07"))
}

// A metadata read that fails has to say which document and why: on a non-OCI
// host this is the error an operator sees, and "404" alone explains nothing.
func TestMetadataErrorsCarryThePathAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	md := &oci.MetadataClient{BaseURL: srv.URL, HTTP: srv.Client()}

	_, err := md.VNICs(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/vnics/")
	assert.Contains(t, err.Error(), "404")
}

// A MAC-cloned bridge (br-wan carries enp1s0's MAC on the reference host) must
// resolve to the same VNIC as the NIC inside it — that is the whole reason
// matching is on MAC rather than on Oracle's VNIC ordering, which is not a
// documented contract and puts the external VNIC second only by chance.
func TestMatchVNICResolvesTheClonedMACToTheExternalVNIC(t *testing.T) {
	md, _ := testMetadata(t)
	vnics, err := md.VNICs(context.Background())
	require.NoError(t, err)

	got, err := oci.MatchVNICByMAC(vnics, "02:00:17:01:7e:71", "br-wan")
	require.NoError(t, err)
	assert.Equal(t, "10.200.0.29", got.PrivateIP)
	assert.Equal(t, vnics[1].VNICID, got.VNICID)
}

func TestMatchVNICNamesTheAttachedMACsWhenItCannotMatch(t *testing.T) {
	md, _ := testMetadata(t)
	vnics, err := md.VNICs(context.Background())
	require.NoError(t, err)

	_, err = oci.MatchVNICByMAC(vnics, "aa:bb:cc:dd:ee:ff", "eth9")
	require.Error(t, err)
	// The operator's next move is comparing MACs, so the error has to show them.
	assert.Contains(t, err.Error(), "eth9")
	assert.Contains(t, err.Error(), "02:00:17:01:7E:71")
}

func TestResolveVNICRejectsAnUnknownInterface(t *testing.T) {
	md, _ := testMetadata(t)

	_, err := oci.ResolveVNICByInterface(context.Background(), md, "definitely-not-a-nic")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "definitely-not-a-nic")
}
