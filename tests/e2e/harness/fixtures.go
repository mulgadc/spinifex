//go:build e2e

// Memoized, cleanup-aware resource fixtures for the e2e harness.
//
// Ensure* helpers are idempotent: the first call creates the resource and
// registers teardown; subsequent calls return the cached ID. Concurrent callers
// share one create via singleflight. Cleanup routes to the Fixture's parent test
// so memoized resources outlive child-subtest lifetimes.
package harness

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"golang.org/x/sync/singleflight"
)

// Fixture carries the per-process e2e state: AWS client surface, memoized
// resource IDs, and the cleanup chain that runs at teardown.
//
// Two construction modes:
//
//   - NewFixture(t, aws): bound to t — cleanups route through t.Cleanup so
//     the standard testing lifecycle owns teardown. Use under an umbrella
//     test or TestMain wrapper that itself owns a *testing.T.
//   - NewProcessFixture(aws): no *testing.T — cleanups queue on an internal
//     LIFO that Close() drains. Use under TestMain where the singleton
//     spans every top-level Test* in the package.
//
// All Ensure* calls within the same Fixture share the memo so a second
// EnsureAMI returns the cached image and never double-builds.
type Fixture struct {
	// parent is the testing.T that constructed the Fixture in test mode.
	// nil in process mode (NewProcessFixture). Cleanup routing forks on
	// this — see registerCleanup.
	parent *testing.T

	EC2   ec2iface.EC2API
	ELBv2 elbv2iface.ELBV2API

	// scratch is a per-process random suffix appended to every resource
	// name created via Ensure*. Prevents AWS-side name collisions when
	// two CI runs share an account (key pair / SG / AMI names are
	// namespace-flat).
	scratch string

	// runID is the cross-process run id (SPINIFEX_E2E_RUN_ID). When set,
	// Ensure* stamps the e2e:run=<runID> tag on every resource it creates so
	// the teardown sweep can reclaim them by id. Empty disables tagging.
	runID string

	mu       sync.Mutex
	memo     map[string]string
	cleanups map[string]struct{}
	sf       singleflight.Group

	// processCleanups holds teardown callbacks for process-mode fixtures.
	// Close() runs them LIFO. Unused (and untouched) when parent != nil.
	//
	// leaks accumulates teardown failures for Close() to return; test-mode
	// fixtures report them on the bound *testing.T instead.
	processMu       sync.Mutex
	processCleanups []func()
	leaks           []string
}

// Compile-time interface checks.
var (
	_ ec2iface.EC2API     = (*ec2.EC2)(nil)
	_ elbv2iface.ELBV2API = (*elbv2.ELBV2)(nil)
)

// NewFixture builds a Fixture bound to t. t.Cleanup hooks register against
// this parent — pass the longest-lived test in your process (TestMain wrapper
// or top-level umbrella Test).
func NewFixture(t *testing.T, aws *AWSClient) *Fixture {
	t.Helper()
	if aws == nil {
		t.Fatalf("NewFixture: nil AWSClient")
	}
	return newFixture(t, aws.EC2, aws.ELBv2)
}

// NewProcessFixture builds a Fixture that lives for the test process. No
// *testing.T parent — Close() must be invoked from TestMain after m.Run()
// to drain the cleanup chain. Use when a singleton fixture must outlive
// every top-level Test* in the package.
func NewProcessFixture(aws *AWSClient) (*Fixture, error) {
	if aws == nil {
		return nil, fmt.Errorf("NewProcessFixture: nil AWSClient")
	}
	scratch, err := randHex(4)
	if err != nil {
		return nil, fmt.Errorf("NewProcessFixture: scratch suffix: %w", err)
	}
	return &Fixture{
		parent:   nil,
		EC2:      aws.EC2,
		ELBv2:    aws.ELBv2,
		scratch:  scratch,
		runID:    os.Getenv(RunIDEnv),
		memo:     map[string]string{},
		cleanups: map[string]struct{}{},
	}, nil
}

// newFixture is the test-facing constructor: callers inject EC2 / ELBv2
// interfaces directly so unit tests can substitute fakes without building a
// real AWSClient (which requires TLS material + env).
func newFixture(t *testing.T, ec2c ec2iface.EC2API, elbc elbv2iface.ELBV2API) *Fixture {
	t.Helper()
	scratch, err := randHex(4)
	if err != nil {
		t.Fatalf("NewFixture: scratch suffix: %v", err)
	}
	return &Fixture{
		parent:   t,
		EC2:      ec2c,
		ELBv2:    elbc,
		scratch:  scratch,
		runID:    os.Getenv(RunIDEnv),
		memo:     map[string]string{},
		cleanups: map[string]struct{}{},
	}
}

// Close drains the process-mode cleanup chain LIFO and returns a non-nil error
// naming every resource it could not tear down, so a caller in TestMain can
// fail the run. No-op returning nil for test-mode fixtures (parent != nil)
// since those route teardown through t.Cleanup.
// Safe to call multiple times; subsequent calls drain an empty slice.
func (f *Fixture) Close() error {
	f.processMu.Lock()
	cleanups := f.processCleanups
	f.processCleanups = nil
	f.processMu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}

	f.processMu.Lock()
	leaks := f.leaks
	f.leaks = nil
	f.processMu.Unlock()
	if len(leaks) == 0 {
		return nil
	}
	return fmt.Errorf("%d resource(s) left behind:\n  %s", len(leaks), strings.Join(leaks, "\n  "))
}

// registerCleanup routes a teardown callback to the appropriate sink based
// on construction mode. Test mode: t.Cleanup on the bound parent. Process
// mode: append to the LIFO drained by Close().
//
// Each callback is isolated from the rest: a panic in one teardown would
// otherwise abort the whole chain and strand every resource behind it.
func (f *Fixture) registerCleanup(fn func()) {
	isolated := func() {
		defer func() {
			if r := recover(); r != nil {
				f.reportLeak("cleanup panicked: %v", r)
			}
		}()
		fn()
	}
	if f.parent != nil {
		f.parent.Cleanup(isolated)
		return
	}
	f.processMu.Lock()
	f.processCleanups = append(f.processCleanups, isolated)
	f.processMu.Unlock()
}

// reportLeak records a teardown failure. A cleanup that fails has left a real
// resource behind on the node, so this must not be a log line nobody reads:
// test-mode fixtures fail the bound test, process-mode fixtures accumulate the
// failures for Close() to return.
func (f *Fixture) reportLeak(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if f.parent != nil {
		f.parent.Errorf("harness.Fixture: LEAKED resource, teardown failed: %s", msg)
		return
	}
	fmt.Fprintf(os.Stderr, "harness.Fixture: LEAKED resource, teardown failed: %s\n", msg)
	f.processMu.Lock()
	f.leaks = append(f.leaks, msg)
	f.processMu.Unlock()
}

// logf forwards diagnostic output to the bound parent in test mode, or to
// stderr in process mode where no *testing.T is available.
func (f *Fixture) logf(format string, args ...any) {
	if f.parent != nil {
		f.parent.Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "harness.Fixture: "+format+"\n", args...)
}

// Scratch returns the per-process suffix appended to resource names. Useful
// for tests that need to assert against names they didn't construct directly.
func (f *Fixture) Scratch() string { return f.scratch }

// RegisterCleanup runs fn at fixture teardown, not at the end of the
// calling subtest. Use for resources created mid-suite by code paths that
// don't flow through ensureOnce (e.g. IAM users threaded across multiple
// phase subtests) — registering on the calling subtest would tear them
// down before the next dependent subtest runs.
//
// Test mode (NewFixture): fn runs at parent t.Cleanup.
// Process mode (NewProcessFixture): fn runs at f.Close() (typically called
// from TestMain after m.Run()).
func (f *Fixture) RegisterCleanup(fn func()) {
	f.registerCleanup(fn)
}

// ensureOnce returns the cached resource ID for key, or calls create exactly
// once (via singleflight) and caches the result. On failure no cleanup is
// registered and the cache is untouched, so a retry triggers a fresh create.
func (f *Fixture) ensureOnce(t *testing.T, key string, create func() (string, func() error, error)) (string, error) {
	t.Helper()

	f.mu.Lock()
	if id, ok := f.memo[key]; ok {
		f.mu.Unlock()
		return id, nil
	}
	f.mu.Unlock()

	v, err, _ := f.sf.Do(key, func() (any, error) {
		f.mu.Lock()
		if id, ok := f.memo[key]; ok {
			f.mu.Unlock()
			return id, nil
		}
		f.mu.Unlock()

		id, cleanup, err := create()
		if err != nil {
			return "", err
		}

		f.mu.Lock()
		f.memo[key] = id
		_, dup := f.cleanups[key]
		if !dup {
			f.cleanups[key] = struct{}{}
		}
		f.mu.Unlock()

		if !dup && cleanup != nil {
			f.registerCleanup(func() {
				if cerr := cleanup(); cerr != nil {
					f.reportLeak("%s (%s): %v", key, id, cerr)
				}
			})
		}
		return id, nil
	})
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	return s, nil
}

// resourceName returns "<prefix>-<scratch>" — the standard naming pattern
// for fixture-created resources. The scratch suffix isolates parallel CI
// runs that share a single AWS account.
func (f *Fixture) resourceName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, f.scratch)
}

// pollUntil polls cond at interval until it returns true or timeout elapses.
// Uses the interface-typed EC2 client so unit-test fakes can drive it.
func pollUntil(t *testing.T, timeout, interval time.Duration, cond func() (bool, error)) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := cond()
		if ok {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timeout after %s: %w", timeout, lastErr)
			}
			return fmt.Errorf("timeout after %s", timeout)
		}
		time.Sleep(interval)
	}
}

func randHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ----------------------------------------------------------------------------
// EnsureKeyPair
// ----------------------------------------------------------------------------

// EnsureKeyPair creates (or returns the cached) named EC2 key pair. The PEM
// is written to artifactsDir/<name>.pem with 0600. Returns (keyName, pemPath).
func EnsureKeyPair(t *testing.T, fx *Fixture, artifactsDir string) (string, string) {
	t.Helper()
	name := fx.resourceName("e2e-key")
	key := "keypair:" + name

	pemPath := filepath.Join(artifactsDir, name+".pem")
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		out, err := fx.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{
			KeyName: aws.String(name),
		})
		if err != nil {
			return "", nil, fmt.Errorf("CreateKeyPair %s: %w", name, err)
		}
		if err := os.MkdirAll(artifactsDir, 0o750); err != nil {
			return "", nil, fmt.Errorf("mkdir %s: %w", artifactsDir, err)
		}
		if err := os.WriteFile(pemPath, []byte(aws.StringValue(out.KeyMaterial)), 0o600); err != nil {
			return "", nil, fmt.Errorf("write pem %s: %w", pemPath, err)
		}
		fx.tagRunResources(aws.StringValue(out.KeyPairId))
		return aws.StringValue(out.KeyName), func() error {
			_, derr := fx.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(name)})
			return derr
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	return id, pemPath
}

// ----------------------------------------------------------------------------
// EnsureAMI
// ----------------------------------------------------------------------------

// AMISource describes how to materialise an AMI. Set exactly one of Existing
// (pre-baked image ID) or CreateFrom (drives CreateImage from a source instance).
type AMISource struct {
	// Existing is a pre-baked AMI ID to verify + return as-is.
	Existing string
	// CreateFrom drives CreateImage from a source instance ID.
	CreateFrom *AMICreateSpec
}

type AMICreateSpec struct {
	SourceInstanceID string
	Name             string
	Description      string
	Architecture     string
	// NoReboot mirrors CreateImage's NoReboot flag. Set true when the source
	// instance must stay running across the bake.
	NoReboot bool
}

// EnsureAMI returns an available AMI ID. Existing IDs short-circuit to a
// state poll; CreateFrom drives CreateImage + waits for "available".
func EnsureAMI(t *testing.T, fx *Fixture, src AMISource) string {
	t.Helper()
	if src.Existing == "" && src.CreateFrom == nil {
		t.Fatalf("EnsureAMI: AMISource requires Existing or CreateFrom")
	}

	key := "ami:"
	if src.Existing != "" {
		key += src.Existing
	} else {
		key += "create:" + src.CreateFrom.SourceInstanceID + ":" + src.CreateFrom.Name
	}

	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		var imageID string
		if src.Existing != "" {
			imageID = src.Existing
		} else {
			spec := src.CreateFrom
			out, err := fx.EC2.CreateImage(&ec2.CreateImageInput{
				InstanceId:  aws.String(spec.SourceInstanceID),
				Name:        aws.String(spec.Name),
				Description: aws.String(spec.Description),
				NoReboot:    aws.Bool(spec.NoReboot),
			})
			if err != nil {
				return "", nil, fmt.Errorf("CreateImage: %w", err)
			}
			imageID = aws.StringValue(out.ImageId)
			fx.tagRunResources(imageID)
		}

		if err := pollUntil(t, 10*time.Minute, 2*time.Second, func() (bool, error) {
			out, err := fx.EC2.DescribeImages(&ec2.DescribeImagesInput{
				ImageIds: []*string{aws.String(imageID)},
			})
			if err != nil {
				return false, err
			}
			if len(out.Images) == 0 {
				return false, fmt.Errorf("image %s not found", imageID)
			}
			state := aws.StringValue(out.Images[0].State)
			if state == "available" {
				return true, nil
			}
			if state == "failed" {
				return false, fmt.Errorf("image %s failed", imageID)
			}
			return false, fmt.Errorf("image %s state=%s", imageID, state)
		}); err != nil {
			return "", nil, err
		}

		cleanup := func() error { return nil }
		if src.CreateFrom != nil {
			cleanup = func() error {
				_, derr := fx.EC2.DeregisterImage(&ec2.DeregisterImageInput{
					ImageId: aws.String(imageID),
				})
				return derr
			}
		}
		return imageID, cleanup, nil
	})
	if err != nil {
		t.Fatalf("EnsureAMI: %v", err)
	}
	return id
}

// ----------------------------------------------------------------------------
// EnsureDefaultVPC
// ----------------------------------------------------------------------------

// VPCInfo bundles the discovered default VPC's IDs.
type VPCInfo struct {
	VPCID    string
	SubnetID string
	SGID     string
}

// EnsureDefaultVPC discovers the default VPC, default subnet (first AZ), and
// default security group. Memoized — repeated calls in the same Fixture hit
// the cache.
func EnsureDefaultVPC(t *testing.T, fx *Fixture) VPCInfo {
	t.Helper()
	key := "default-vpc"
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		vpcs, err := fx.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{
			Filters: []*ec2.Filter{{
				// Filter name is `is-default` per the AWS EC2 surface — the
				// daemon rejects the camelCase `isDefault` form with a 400
				// InvalidParameterValue (matches the AWS CLI contract).
				Name:   aws.String("is-default"),
				Values: []*string{aws.String("true")},
			}},
		})
		if err != nil {
			return "", nil, fmt.Errorf("DescribeVpcs: %w", err)
		}
		if len(vpcs.Vpcs) == 0 {
			return "", nil, fmt.Errorf("no default VPC")
		}
		vpcID := aws.StringValue(vpcs.Vpcs[0].VpcId)
		return vpcID, nil, nil
	})
	if err != nil {
		t.Fatalf("EnsureDefaultVPC: %v", err)
	}

	subnetKey := "default-subnet:" + id
	subnetID, err := fx.ensureOnce(t, subnetKey, func() (string, func() error, error) {
		out, err := fx.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{
			Filters: []*ec2.Filter{{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(id)},
			}},
		})
		if err != nil {
			return "", nil, fmt.Errorf("DescribeSubnets: %w", err)
		}
		if len(out.Subnets) == 0 {
			return "", nil, fmt.Errorf("no subnets in default VPC %s", id)
		}
		return aws.StringValue(out.Subnets[0].SubnetId), nil, nil
	})
	if err != nil {
		t.Fatalf("EnsureDefaultVPC subnet: %v", err)
	}

	sgKey := "default-sg:" + id
	sgID, err := fx.ensureOnce(t, sgKey, func() (string, func() error, error) {
		out, err := fx.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
			Filters: []*ec2.Filter{
				{Name: aws.String("vpc-id"), Values: []*string{aws.String(id)}},
				{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
			},
		})
		if err != nil {
			return "", nil, fmt.Errorf("DescribeSecurityGroups: %w", err)
		}
		if len(out.SecurityGroups) == 0 {
			return "", nil, fmt.Errorf("no default SG in VPC %s", id)
		}
		return aws.StringValue(out.SecurityGroups[0].GroupId), nil, nil
	})
	if err != nil {
		t.Fatalf("EnsureDefaultVPC sg: %v", err)
	}

	return VPCInfo{VPCID: id, SubnetID: subnetID, SGID: sgID}
}

// ----------------------------------------------------------------------------
// EnsureSubnet
// ----------------------------------------------------------------------------

// EnsureSubnet creates a new subnet in vpcID with cidr in az. Returns the
// subnet ID. Memoized by (vpcID, cidr) — repeated calls with the same
// arguments return the cached subnet.
func EnsureSubnet(t *testing.T, fx *Fixture, vpcID, cidr, az string) string {
	t.Helper()
	key := fmt.Sprintf("subnet:%s:%s", vpcID, cidr)
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		input := &ec2.CreateSubnetInput{
			VpcId:     aws.String(vpcID),
			CidrBlock: aws.String(cidr),
		}
		if az != "" {
			input.AvailabilityZone = aws.String(az)
		}
		out, err := fx.EC2.CreateSubnet(input)
		if err != nil {
			return "", nil, fmt.Errorf("CreateSubnet %s: %w", cidr, err)
		}
		subnetID := aws.StringValue(out.Subnet.SubnetId)
		fx.tagRunResources(subnetID)
		return subnetID, func() error {
			_, derr := fx.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{
				SubnetId: aws.String(subnetID),
			})
			return derr
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureSubnet: %v", err)
	}
	return id
}

// ----------------------------------------------------------------------------
// EnsureSG
// ----------------------------------------------------------------------------

// EnsureSG creates (or returns the cached) named security group in vpcID.
func EnsureSG(t *testing.T, fx *Fixture, vpcID, namePrefix string) string {
	t.Helper()
	name := fx.resourceName(namePrefix)
	key := fmt.Sprintf("sg:%s:%s", vpcID, name)
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		out, err := fx.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
			VpcId:       aws.String(vpcID),
			GroupName:   aws.String(name),
			Description: aws.String("e2e fixture SG " + name),
		})
		if err != nil {
			return "", nil, fmt.Errorf("CreateSecurityGroup %s: %w", name, err)
		}
		sgID := aws.StringValue(out.GroupId)
		fx.tagRunResources(sgID)
		return sgID, func() error {
			_, derr := fx.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
				GroupId: aws.String(sgID),
			})
			return derr
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureSG: %v", err)
	}
	return id
}

// ----------------------------------------------------------------------------
// EnsureInstance
// ----------------------------------------------------------------------------

// InstanceSpec captures the inputs to RunInstances. UserData is optional.
type InstanceSpec struct {
	AMIID        string
	InstanceType string
	KeyName      string
	SubnetID     string
	SGID         string
	UserData     string
}

// EnsureInstance launches a single instance matching spec, polls to
// "running", registers terminate-on-cleanup. Returns the instance ID.
func EnsureInstance(t *testing.T, fx *Fixture, spec InstanceSpec) string {
	t.Helper()
	// Memo key must include every input that changes the running instance —
	// otherwise a second EnsureInstance call with a different SGID/UserData
	// would silently return the first instance's ID.
	udHash := ""
	if spec.UserData != "" {
		sum := sha256.Sum256([]byte(spec.UserData))
		udHash = hex.EncodeToString(sum[:8])
	}
	key := fmt.Sprintf("instance:%s:%s:%s:%s:%s:%s",
		spec.AMIID, spec.InstanceType, spec.KeyName, spec.SubnetID, spec.SGID, udHash)
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		input := &ec2.RunInstancesInput{
			ImageId:      aws.String(spec.AMIID),
			InstanceType: aws.String(spec.InstanceType),
			KeyName:      aws.String(spec.KeyName),
			SubnetId:     aws.String(spec.SubnetID),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		}
		if spec.SGID != "" {
			input.SecurityGroupIds = []*string{aws.String(spec.SGID)}
		}
		if spec.UserData != "" {
			input.UserData = aws.String(spec.UserData)
		}
		out, err := fx.EC2.RunInstances(input)
		if err != nil {
			return "", nil, fmt.Errorf("RunInstances: %w", err)
		}
		if len(out.Instances) == 0 {
			return "", nil, fmt.Errorf("RunInstances returned 0 instances")
		}
		instID := aws.StringValue(out.Instances[0].InstanceId)
		fx.tagRunResources(instID)

		if err := pollUntil(t, 5*time.Minute, 2*time.Second, func() (bool, error) {
			d, derr := fx.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
				InstanceIds: []*string{aws.String(instID)},
			})
			if derr != nil {
				return false, derr
			}
			if len(d.Reservations) == 0 || len(d.Reservations[0].Instances) == 0 {
				return false, fmt.Errorf("instance %s not found", instID)
			}
			state := aws.StringValue(d.Reservations[0].Instances[0].State.Name)
			if state == "running" {
				return true, nil
			}
			return false, fmt.Errorf("instance %s state=%s", instID, state)
		}); err != nil {
			return "", nil, err
		}

		return instID, func() error {
			return teardownInstance(fx.EC2, instID)
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureInstance: %v", err)
	}
	return id
}

// ----------------------------------------------------------------------------
// EnsureVolume
// ----------------------------------------------------------------------------

// EnsureVolume creates a standalone EBS volume in az of sizeGiB, polls to
// "available", returns the volume ID.
func EnsureVolume(t *testing.T, fx *Fixture, az string, sizeGiB int64) string {
	t.Helper()
	key := fmt.Sprintf("volume:%s:%d", az, sizeGiB)
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		out, err := fx.EC2.CreateVolume(&ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az),
			Size:             aws.Int64(sizeGiB),
		})
		if err != nil {
			return "", nil, fmt.Errorf("CreateVolume: %w", err)
		}
		volID := aws.StringValue(out.VolumeId)
		fx.tagRunResources(volID)

		if err := pollUntil(t, 5*time.Minute, 2*time.Second, func() (bool, error) {
			d, derr := fx.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
				VolumeIds: []*string{aws.String(volID)},
			})
			if derr != nil {
				return false, derr
			}
			if len(d.Volumes) == 0 {
				return false, fmt.Errorf("volume %s not found", volID)
			}
			state := aws.StringValue(d.Volumes[0].State)
			if state == "available" {
				return true, nil
			}
			return false, fmt.Errorf("volume %s state=%s", volID, state)
		}); err != nil {
			return "", nil, err
		}

		return volID, func() error {
			return teardownVolume(fx.EC2, volID)
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	return id
}

// ----------------------------------------------------------------------------
// EnsureSnapshot
// ----------------------------------------------------------------------------

// SnapshotSpec captures the inputs to CreateSnapshot. Description is
// optional — empty means use the EnsureSnapshot default.
type SnapshotSpec struct {
	VolumeID    string
	Description string
}

// EnsureSnapshot creates a snapshot matching spec, polls to "completed",
// returns the snapshot ID.
func EnsureSnapshot(t *testing.T, fx *Fixture, spec SnapshotSpec) string {
	t.Helper()
	desc := spec.Description
	if desc == "" {
		desc = "e2e fixture snapshot for " + spec.VolumeID
	}
	// Memo key includes description so a second EnsureSnapshot call with a
	// distinct description gets its own snapshot (caller intent: distinct
	// resource), not a silent return of the first ID.
	descSum := sha256.Sum256([]byte(desc))
	key := fmt.Sprintf("snapshot:%s:%s", spec.VolumeID, hex.EncodeToString(descSum[:8]))
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		out, err := fx.EC2.CreateSnapshot(&ec2.CreateSnapshotInput{
			VolumeId:    aws.String(spec.VolumeID),
			Description: aws.String(desc),
		})
		if err != nil {
			return "", nil, fmt.Errorf("CreateSnapshot: %w", err)
		}
		snapID := aws.StringValue(out.SnapshotId)
		fx.tagRunResources(snapID)

		if err := pollUntil(t, 10*time.Minute, 2*time.Second, func() (bool, error) {
			d, derr := fx.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
				SnapshotIds: []*string{aws.String(snapID)},
			})
			if derr != nil {
				return false, derr
			}
			if len(d.Snapshots) == 0 {
				return false, fmt.Errorf("snapshot %s not found", snapID)
			}
			state := aws.StringValue(d.Snapshots[0].State)
			if state == "completed" {
				return true, nil
			}
			if state == "error" {
				return false, fmt.Errorf("snapshot %s entered error state", snapID)
			}
			return false, fmt.Errorf("snapshot %s state=%s", snapID, state)
		}); err != nil {
			return "", nil, err
		}

		return snapID, func() error {
			_, derr := fx.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
				SnapshotId: aws.String(snapID),
			})
			return derr
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureSnapshot: %v", err)
	}
	return id
}

// ----------------------------------------------------------------------------
// EnsureNATGateway
// ----------------------------------------------------------------------------

// EnsureNATGateway creates a NAT gateway in subnetID with allocationID,
// polls to "available", returns the NAT gateway ID.
func EnsureNATGateway(t *testing.T, fx *Fixture, subnetID, allocationID string) string {
	t.Helper()
	key := fmt.Sprintf("natgw:%s:%s", subnetID, allocationID)
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		input := &ec2.CreateNatGatewayInput{
			SubnetId: aws.String(subnetID),
		}
		if allocationID != "" {
			input.AllocationId = aws.String(allocationID)
		}
		out, err := fx.EC2.CreateNatGateway(input)
		if err != nil {
			return "", nil, fmt.Errorf("CreateNatGateway: %w", err)
		}
		ngwID := aws.StringValue(out.NatGateway.NatGatewayId)
		fx.tagRunResources(ngwID)

		if err := pollUntil(t, 5*time.Minute, 5*time.Second, func() (bool, error) {
			d, derr := fx.EC2.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
				NatGatewayIds: []*string{aws.String(ngwID)},
			})
			if derr != nil {
				return false, derr
			}
			if len(d.NatGateways) == 0 {
				return false, fmt.Errorf("natgw %s not found", ngwID)
			}
			state := aws.StringValue(d.NatGateways[0].State)
			if state == "available" {
				return true, nil
			}
			if state == "failed" {
				return false, fmt.Errorf("natgw %s entered failed state", ngwID)
			}
			return false, fmt.Errorf("natgw %s state=%s", ngwID, state)
		}); err != nil {
			return "", nil, err
		}

		return ngwID, func() error {
			_, derr := fx.EC2.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
				NatGatewayId: aws.String(ngwID),
			})
			return derr
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureNATGateway: %v", err)
	}
	return id
}

// ----------------------------------------------------------------------------
// EnsureLoadBalancer
// ----------------------------------------------------------------------------

// LoadBalancerSpec captures the inputs to ELBv2 CreateLoadBalancer. Scheme
// is "internet-facing" or "internal"; Type is "application" or "network".
type LoadBalancerSpec struct {
	NamePrefix string
	Scheme     string
	Type       string
	Subnets    []string
	SGs        []string
}

// EnsureLoadBalancer creates an ELBv2 load balancer per spec, polls to
// "active", returns the LB ARN.
func EnsureLoadBalancer(t *testing.T, fx *Fixture, spec LoadBalancerSpec) string {
	t.Helper()
	name := fx.resourceName(spec.NamePrefix)
	key := fmt.Sprintf("lb:%s:%s:%s", spec.Type, spec.Scheme, name)
	id, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		input := &elbv2.CreateLoadBalancerInput{
			Name:    aws.String(name),
			Scheme:  aws.String(spec.Scheme),
			Type:    aws.String(spec.Type),
			Subnets: aws.StringSlice(spec.Subnets),
		}
		if len(spec.SGs) > 0 {
			input.SecurityGroups = aws.StringSlice(spec.SGs)
		}
		out, err := fx.ELBv2.CreateLoadBalancer(input)
		if err != nil {
			return "", nil, fmt.Errorf("CreateLoadBalancer %s: %w", name, err)
		}
		if len(out.LoadBalancers) == 0 {
			return "", nil, fmt.Errorf("CreateLoadBalancer returned 0 LBs")
		}
		lbARN := aws.StringValue(out.LoadBalancers[0].LoadBalancerArn)
		fx.tagRunELB(lbARN)

		if err := pollUntil(t, 10*time.Minute, 5*time.Second, func() (bool, error) {
			d, derr := fx.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
				LoadBalancerArns: []*string{aws.String(lbARN)},
			})
			if derr != nil {
				return false, derr
			}
			if len(d.LoadBalancers) == 0 {
				return false, fmt.Errorf("lb %s not found", lbARN)
			}
			state := aws.StringValue(d.LoadBalancers[0].State.Code)
			if state == "active" {
				return true, nil
			}
			if state == "failed" {
				return false, fmt.Errorf("lb %s entered failed state", lbARN)
			}
			return false, fmt.Errorf("lb %s state=%s", lbARN, state)
		}); err != nil {
			return "", nil, err
		}

		return lbARN, func() error {
			_, derr := fx.ELBv2.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
				LoadBalancerArn: aws.String(lbARN),
			})
			return derr
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureLoadBalancer: %v", err)
	}
	return id
}
