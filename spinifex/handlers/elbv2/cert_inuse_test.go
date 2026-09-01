package handlers_elbv2

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/aws/aws-sdk-go/service/elbv2"
	handlers_acm "github.com/mulgadc/spinifex/spinifex/handlers/acm"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// activeLB persists a LoadBalancerRecord directly with an InstanceID set,
// mirroring the LBAgentHeartbeat test helpers, so updateStoredConfig actually
// renders instead of no-opping on a still-provisioning LB.
func activeLB(t *testing.T, svc *ELBv2ServiceImpl, id, name string) *LoadBalancerRecord {
	t.Helper()
	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/" + name + "/" + id,
		LoadBalancerID:  id,
		Name:            name,
		State:           StateActive,
		InstanceID:      "i-sys-" + id,
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(context.Background(), lb))
	return lb
}

func httpsListenerInput(lbArn, certArn string, port int64) *elbv2.CreateListenerInput {
	return &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(ProtocolHTTPS),
		Port:            aws.Int64(port),
		Certificates:    []*elbv2.Certificate{{CertificateArn: aws.String(certArn)}},
		DefaultActions:  fixedResponseAction(),
	}
}

func TestCreateListener_AddsToInUseByIndex(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-idx1", "idx-lb1")

	_, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, rec.InUseBy)
}

func TestCreateListener_TwoListenersSameCert_SingleInUseByEntry(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-idx2", "idx-lb2")

	_, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 8443), testAccountID)
	require.NoError(t, err)

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, rec.InUseBy,
		"two listeners on the same LB referencing the same cert must dedupe to one entry")
}

func TestDeleteListener_RemovesFromInUseByIndex(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-idx3", "idx-lb3")

	out, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteListener(context.Background(), &elbv2.DeleteListenerInput{
		ListenerArn: out.Listeners[0].ListenerArn,
	}, testAccountID)
	require.NoError(t, err)

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Empty(t, rec.InUseBy, "deleting the sole listener must clear the index entry")
}

func TestDeleteListener_KeepsInUseByWhenAnotherListenerSharesCert(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-idx4", "idx-lb4")

	out1, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 8443), testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteListener(context.Background(), &elbv2.DeleteListenerInput{
		ListenerArn: out1.Listeners[0].ListenerArn,
	}, testAccountID)
	require.NoError(t, err)

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, rec.InUseBy, "the other listener still references the cert")
}

func TestModifyListener_SwitchesCertInInUseByIndex(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-idx5", "idx-lb5")

	out, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)

	_, err = svc.ModifyListener(context.Background(), &elbv2.ModifyListenerInput{
		ListenerArn:  out.Listeners[0].ListenerArn,
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn2)}},
	}, testAccountID)
	require.NoError(t, err)

	oldRec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Empty(t, oldRec.InUseBy, "old cert must drop the LB once no listener references it")

	newRec, err := svc.acmStore.GetCert(context.Background(), testCertArn2)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, newRec.InUseBy)
}

func TestDeleteLoadBalancer_RemovesFromInUseByIndex(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-idx6", "idx-lb6")

	_, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(lb.LoadBalancerArn),
	}, testAccountID)
	require.NoError(t, err)

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Empty(t, rec.InUseBy, "deleting the LB must remove it from the index via the listener cascade")
}

func TestAddRemoveListenerCertificates_MaintainInUseByIndex(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-idx7", "idx-lb7")

	out, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)
	listenerArn := out.Listeners[0].ListenerArn

	_, err = svc.AddListenerCertificates(context.Background(), &elbv2.AddListenerCertificatesInput{
		ListenerArn:  listenerArn,
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn2)}},
	}, testAccountID)
	require.NoError(t, err)

	rec2, err := svc.acmStore.GetCert(context.Background(), testCertArn2)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, rec2.InUseBy)

	_, err = svc.RemoveListenerCertificates(context.Background(), &elbv2.RemoveListenerCertificatesInput{
		ListenerArn:  listenerArn,
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn2)}},
	}, testAccountID)
	require.NoError(t, err)

	rec2, err = svc.acmStore.GetCert(context.Background(), testCertArn2)
	require.NoError(t, err)
	assert.Empty(t, rec2.InUseBy)

	// The default cert (testCertArn) is still referenced.
	rec1, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, rec1.InUseBy)
}

func TestUpdateStoredConfigForCert_ZeroInUseBy_NoOp(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	require.NoError(t, svc.UpdateStoredConfigForCert(context.Background(), testCertArn))
}

func TestUpdateStoredConfigForCert_UnknownCert_NoOp(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	require.NoError(t, svc.UpdateStoredConfigForCert(context.Background(), "arn:aws:acm:ap-southeast-2:000000000001:certificate/does-not-exist"))
}

// TestUpdateStoredConfigForCert_SkipsMissingOrInactiveLB covers both halves of
// the "must not fail the certificate write" edge case: an InUseBy entry whose
// LB was deleted out from under the index, and one whose LB exists but never
// went active (empty InstanceID, so updateStoredConfig itself no-ops).
func TestUpdateStoredConfigForCert_SkipsMissingOrInactiveLB(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	require.NoError(t, svc.acmStore.AddInUseBy(context.Background(), testCertArn,
		"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/gone/lb-gone"))

	provisioning := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/prov/lb-prov1",
		LoadBalancerID:  "lb-prov1",
		Name:            "prov-lb",
		State:           StateProvisioning,
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(context.Background(), provisioning))
	require.NoError(t, svc.acmStore.AddInUseBy(context.Background(), testCertArn, provisioning.LoadBalancerArn))

	require.NoError(t, svc.UpdateStoredConfigForCert(context.Background(), testCertArn))

	stored, err := svc.store.GetLoadBalancer(context.Background(), "lb-prov1")
	require.NoError(t, err)
	assert.Empty(t, stored.ConfigHash, "a still-provisioning LB must not get a config built for it")
}

// clearInUseBy strips a certificate's index entry for lbArn without touching
// its listeners, reproducing a listener that predates the index.
func clearInUseBy(t *testing.T, svc *ELBv2ServiceImpl, certArn, lbArn string) {
	t.Helper()
	require.NoError(t, svc.acmStore.RemoveInUseBy(context.Background(), certArn, lbArn))
	rec, err := svc.acmStore.GetCert(context.Background(), certArn)
	require.NoError(t, err)
	require.Empty(t, rec.InUseBy, "precondition: the index must start empty")
}

func TestReconcileCertInUseIndex_BackfillsPreExistingListener(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-rec1", "rec-lb1")

	_, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)
	clearInUseBy(t, svc, testCertArn, lb.LoadBalancerArn)

	require.NoError(t, svc.ReconcileCertInUseIndex(context.Background()))

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, rec.InUseBy,
		"the listener still references the cert, so the rebuild must restore the entry")
}

func TestReconcileCertInUseIndex_IsIdempotent(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-rec2", "rec-lb2")

	_, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)
	clearInUseBy(t, svc, testCertArn, lb.LoadBalancerArn)

	require.NoError(t, svc.ReconcileCertInUseIndex(context.Background()))
	require.NoError(t, svc.ReconcileCertInUseIndex(context.Background()))

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, rec.InUseBy, "a second run must not duplicate the entry")
}

func TestReconcileCertInUseIndex_RemovesEntryWithNoListener(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-rec3", "rec-lb3")

	// The LB exists but no listener references the cert, so the entry is stale.
	require.NoError(t, svc.acmStore.AddInUseBy(context.Background(), testCertArn, lb.LoadBalancerArn))

	require.NoError(t, svc.ReconcileCertInUseIndex(context.Background()))

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Empty(t, rec.InUseBy, "a rebuild must drop an entry no listener justifies")
}

func TestReconcileCertInUseIndex_RemovesEntryForDeletedLB(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	require.NoError(t, svc.acmStore.AddInUseBy(context.Background(), testCertArn,
		"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/gone/lb-rec-gone"))

	require.NoError(t, svc.ReconcileCertInUseIndex(context.Background()))

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Empty(t, rec.InUseBy, "an entry naming a load balancer that no longer exists must be dropped")
}

func TestReconcileCertInUseIndex_TwoListenersSameLB_SingleEntry(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := activeLB(t, svc, "lb-rec4", "rec-lb4")

	_, err := svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 443), testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, testCertArn, 8443), testAccountID)
	require.NoError(t, err)
	clearInUseBy(t, svc, testCertArn, lb.LoadBalancerArn)

	require.NoError(t, svc.ReconcileCertInUseIndex(context.Background()))

	rec, err := svc.acmStore.GetCert(context.Background(), testCertArn)
	require.NoError(t, err)
	assert.Equal(t, []string{lb.LoadBalancerArn}, rec.InUseBy, "two listeners on one LB must dedupe to one entry")
}

// A nil acmStore means HTTPS is unavailable, not that the index is broken, so
// the reconcile has to no-op rather than panic on daemon startup.
func TestReconcileCertInUseIndex_NilACMStore_NoOp(t *testing.T) {
	t.Parallel()
	require.NoError(t, (&ELBv2ServiceImpl{}).ReconcileCertInUseIndex(context.Background()))
}

// TestReconcileCertInUseIndex_RestoresRenewalFanOut is the regression test for
// the reported symptom rather than the mechanism: with the index emptied, a
// re-import reaches no load balancer and the served leaf goes stale silently.
// After a reconcile the same re-import must fan out again.
func TestReconcileCertInUseIndex_RestoresRenewalFanOut(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	elbv2Svc, err := NewELBv2ServiceImplWithNATS(nil, nc, masterKey)
	require.NoError(t, err)
	acmSvc, err := handlers_acm.NewACMServiceImplWithNATS(context.Background(), nil, nc, masterKey)
	require.NoError(t, err)
	acmSvc.CertMaterialUpdated = elbv2Svc.UpdateStoredConfigForCert

	origLeaf, origKey := genLeafCertPEM(t, "stale.example.com")
	importOut, err := acmSvc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: origLeaf,
		PrivateKey:  origKey,
	}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(importOut.CertificateArn)

	lb := activeLB(t, elbv2Svc, "lb-rec5", "rec-lb5")
	_, err = elbv2Svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, certArn, 443), testAccountID)
	require.NoError(t, err)
	clearInUseBy(t, elbv2Svc, certArn, lb.LoadBalancerArn)

	stale, err := elbv2Svc.store.GetLoadBalancer(context.Background(), "lb-rec5")
	require.NoError(t, err)

	// Without an index entry the re-import succeeds but reaches nothing.
	midLeaf, midKey := genLeafCertPEM(t, "ignored.example.com")
	_, err = acmSvc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    midLeaf,
		PrivateKey:     midKey,
		CertificateArn: aws.String(certArn),
	}, testAccountID)
	require.NoError(t, err)

	unchanged, err := elbv2Svc.store.GetLoadBalancer(context.Background(), "lb-rec5")
	require.NoError(t, err)
	require.Equal(t, stale.ConfigHash, unchanged.ConfigHash,
		"precondition: an empty index is exactly why renewal goes unnoticed")

	require.NoError(t, elbv2Svc.ReconcileCertInUseIndex(context.Background()))

	newLeaf, newKey := genLeafCertPEM(t, "renewed.example.com")
	_, err = acmSvc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    newLeaf,
		PrivateKey:     newKey,
		CertificateArn: aws.String(certArn),
	}, testAccountID)
	require.NoError(t, err)

	after, err := elbv2Svc.store.GetLoadBalancer(context.Background(), "lb-rec5")
	require.NoError(t, err)
	assert.NotEqual(t, unchanged.ConfigHash, after.ConfigHash, "the reconciled index must let the fan-out through")

	var afterPEM string
	for _, pemContent := range after.CertFiles {
		afterPEM = pemContent
	}
	assert.Contains(t, afterPEM, string(newLeaf), "the rendered cert file must carry the renewed leaf")
}

// genLeafCertPEM returns a self-signed leaf certificate + private key as PEM,
// mirroring handlers_acm's own test helper (unexported, different package).
// dnsNames become SANs, which is how a renderer learns the names a cert serves.
func genLeafCertPEM(t *testing.T, cn string, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// TestACMReimport_FansOutToInUseLoadBalancer is the end-to-end regression test
// for the silent re-import bug: ACMServiceImpl.ImportCertificate on an
// existing, in-use ARN must fan out to every load balancer referencing it and
// re-render its stored config, exactly as wired in daemon.go. It exercises
// the real ImportCertificate path (not a direct store write), the real
// CreateListener path (which builds the InUseBy index), and the real
// UpdateStoredConfigForCert fan-out.
func TestACMReimport_FansOutToInUseLoadBalancer(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)

	// Both services read the same bucket, so they must share one key or the
	// reader gets ciphertext where it expects PEM.
	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	elbv2Svc, err := NewELBv2ServiceImplWithNATS(nil, nc, masterKey)
	require.NoError(t, err)
	acmSvc, err := handlers_acm.NewACMServiceImplWithNATS(context.Background(), nil, nc, masterKey)
	require.NoError(t, err)
	// Mirrors the daemon.go wiring between the two services.
	acmSvc.CertMaterialUpdated = elbv2Svc.UpdateStoredConfigForCert

	origLeaf, origKey := genLeafCertPEM(t, "orig.example.com")
	importOut, err := acmSvc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: origLeaf,
		PrivateKey:  origKey,
	}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(importOut.CertificateArn)

	lb := activeLB(t, elbv2Svc, "lb-reimport1", "reimport-lb")

	_, err = elbv2Svc.CreateListener(context.Background(), httpsListenerInput(lb.LoadBalancerArn, certArn, 443), testAccountID)
	require.NoError(t, err)

	before, err := elbv2Svc.store.GetLoadBalancer(context.Background(), "lb-reimport1")
	require.NoError(t, err)
	require.NotEmpty(t, before.ConfigHash, "CreateListener must have rendered a config")

	var beforePEM string
	for _, pemContent := range before.CertFiles {
		beforePEM = pemContent
	}
	assert.Contains(t, beforePEM, string(origLeaf))

	// Re-import new leaf material under the same ARN — the bug being fixed.
	newLeaf, newKey := genLeafCertPEM(t, "rotated.example.com")
	_, err = acmSvc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    newLeaf,
		PrivateKey:     newKey,
		CertificateArn: aws.String(certArn),
	}, testAccountID)
	require.NoError(t, err)

	after, err := elbv2Svc.store.GetLoadBalancer(context.Background(), "lb-reimport1")
	require.NoError(t, err)
	assert.NotEqual(t, before.ConfigHash, after.ConfigHash, "ConfigHash must change after re-import fans out")

	var afterPEM string
	for _, pemContent := range after.CertFiles {
		afterPEM = pemContent
	}
	assert.Contains(t, afterPEM, string(newLeaf), "the rendered cert file must carry the new leaf")
	assert.NotContains(t, afterPEM, string(origLeaf), "the old leaf must not still be served")
}

// Concurrency coverage for the InUseBy index itself — N goroutines each
// adding a distinct resource ARN to the same certificate, asserting the
// final set contains exactly all N — lives at the store layer, where the
// property actually belongs: see
// TestAddInUseBy_ConcurrentDistinctResourcesAllSurvive in
// handlers/acm/store_test.go. An earlier version of this test asserted only
// "no errors/panics" across a concurrent re-import/ModifyListener race,
// which cannot detect a lost index entry: a dropped update produces a
// correct-looking record with a missing entry, not an error, so that
// assertion passed whether or not the underlying race was fixed.
