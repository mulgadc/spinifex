// Characterization tests for EC2 through the full SigV4 pipeline as the legacy
// gateway switch and ec2.go implement them today. They assert current
// behaviour, not AWS parity.

package gateway

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/ratelimit"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ec2Daemon stands in for the daemons behind the ec2.* NATS subjects. It
// records what the gateway forwarded and answers every subject with an empty
// output unless a case needs a real shape.
type ec2Daemon struct {
	mu     sync.Mutex
	calls  map[string]int
	bodies map[string][]byte
	eigw   int
	// replies overrides the canned answer for a subject.
	replies map[string]string
}

func (d *ec2Daemon) reply(subject, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.replies[subject] = body
}

func newEC2Daemon(t *testing.T, nc *nats.Conn) *ec2Daemon {
	t.Helper()
	d := &ec2Daemon{calls: map[string]int{}, bodies: map[string][]byte{}, replies: map[string]string{}}
	sub, err := nc.Subscribe("ec2.>", func(m *nats.Msg) {
		d.mu.Lock()
		d.calls[m.Subject]++
		d.bodies[m.Subject] = append([]byte(nil), m.Data...)
		reply := `{}`
		switch m.Subject {
		case "ec2.CreateEgressOnlyInternetGateway":
			d.eigw++
			reply = fmt.Sprintf(`{"EgressOnlyInternetGateway":{"EgressOnlyInternetGatewayId":"eigw-%04d"}}`, d.eigw)
		case "ec2.DescribeVpcs":
			reply = `{"Vpcs":[]}`
		}
		if r, ok := d.replies[m.Subject]; ok {
			reply = r
		}
		d.mu.Unlock()
		_ = m.Respond([]byte(reply))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())
	return d
}

func (d *ec2Daemon) count(subject string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[subject]
}

func (d *ec2Daemon) body(subject string) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bodies[subject]
}

// ec2PipelineEnv is what a case may change before the app is built.
type ec2PipelineEnv struct {
	NoNATS     bool
	ResolveErr error
	// Outer runs ahead of SigV4, as the telemetry and audit middleware do.
	Outer []func(http.Handler) http.Handler
	// Throttler, when set, is installed after the action enricher as SetupRoutes does.
	Throttler *ratelimit.Throttler
}

type ec2PipelineApp struct {
	Handler http.Handler
	Daemon  *ec2Daemon
}

// ec2Pipeline wires SigV4AuthMiddleware -> Request as SetupRoutes does, with
// EC2 on the legacy switch over a real JetStream and a stub daemon. Policy
// statements are the caller's only IAM policy.
func ec2Pipeline(t *testing.T, mutate func(*ec2PipelineEnv), statements ...iamdomain.Statement) ec2PipelineApp {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	env := &ec2PipelineEnv{}
	if mutate != nil {
		mutate(env)
	}
	svc := &profileIAMService{resolveErr: env.ResolveErr, PolicyIAM: gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       authzAccountID,
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
	}}
	_, nc, _ := testutil.StartTestJetStream(t)
	daemon := newEC2Daemon(t, nc)

	cfg := &GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		NATSConn:       nc,
		IAMService:     svc,
		ExpectedNodes:  1,
	}
	if env.NoNATS {
		cfg.NATSConn = nil
	}
	gw := withEC2(withVerifier(cfg), func(d *ec2awsapi.Deps) {
		d.AZ = gwfix.Region + "a"
		d.Endpoint = "https://registry.example"
	})
	r := chi.NewRouter()
	for _, mw := range env.Outer {
		r.Use(mw)
	}
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(TraceActionEnricher)
	if env.Throttler != nil {
		r.Use(env.Throttler.Middleware(gw.throttleKeyFuncs(), gw.writeThrottleError))
	}
	r.HandleFunc("/*", gw.Request)
	return ec2PipelineApp{Handler: r, Daemon: daemon}
}

func signEC2Request(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	gwfix.SignRequestAs(t, req, []byte(body), gwfix.AccessKey, gwfix.SecretKey, "ec2", time.Now().UTC())
	return req
}

type ec2ErrorBody struct {
	XMLName xml.Name `xml:"Response"`
	Errors  struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	} `xml:"Errors"`
	RequestID string `xml:"RequestID"`
}

type ec2Outcome struct {
	Status      int
	ContentType string
	Body        string
	Code        string
	Message     string
	RequestID   string
}

func probeEC2(t *testing.T, app http.Handler, body string) ec2Outcome {
	t.Helper()
	resp := doRequest(app, signEC2Request(t, body))
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := ec2Outcome{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: string(raw)}
	if resp.StatusCode >= 400 {
		var env ec2ErrorBody
		require.NoError(t, xml.Unmarshal(raw, &env), string(raw))
		out.Code, out.Message, out.RequestID = env.Errors.Error.Code, env.Errors.Error.Message, env.RequestID
	}
	return out
}

func allowEC2() iamdomain.Statement { return statement("Allow", "ec2:*", "*") }

// TestEC2Pipeline_ErrorEnvelopes pins status, code and the EC2 error envelope
// for each way a request can be refused before or at dispatch.
func TestEC2Pipeline_ErrorEnvelopes(t *testing.T) {
	cases := []struct {
		name   string
		noNATS bool
		deny   bool
		body   string
		status int
		code   string
	}{
		{name: "missing action", body: "Version=2016-11-15", status: 400, code: "MissingAction"},
		{name: "invalid action", body: "Action=NoSuchAction", status: 400, code: "InvalidAction"},
		{name: "policy denial is UnauthorizedOperation", deny: true, body: "Action=DescribeKeyPairs", status: 403, code: "UnauthorizedOperation"},
		{name: "dry run", body: "Action=DescribeKeyPairs&DryRun=true", status: 412, code: "DryRunOperation"},
		{name: "max results out of range", body: "Action=DescribeInstances&MaxResults=1", status: 400, code: "InvalidParameterValue"},
		{name: "tag resource type not taggable", body: "Action=CreateKeyPair&KeyName=k&TagSpecification.1.ResourceType=instance", status: 400, code: "InvalidParameterValue"},
		{name: "cluster unavailable, non-local action", noNATS: true, body: "Action=DescribeKeyPairs", status: 503, code: "ServiceUnavailable"},
		{name: "cluster unavailable, local action", noNATS: true, body: "Action=DescribeRegions", status: 503, code: "ServiceUnavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stmts []iamdomain.Statement
			if !c.deny {
				stmts = append(stmts, allowEC2())
			}
			app := ec2Pipeline(t, func(e *ec2PipelineEnv) { e.NoNATS = c.noNATS }, stmts...)
			got := probeEC2(t, app.Handler, c.body)
			require.Equal(t, c.status, got.Status, got.Body)
			assert.Equal(t, c.code, got.Code, got.Body)
			assert.NotEmpty(t, got.RequestID, "error body carries a request id: %s", got.Body)
			assert.True(t, strings.HasPrefix(got.Body, "<?xml"), got.Body)
			assert.Contains(t, got.ContentType, "xml")
		})
	}
}

// TestEC2Pipeline_CheckOrder pins which refusal wins when a request earns
// several: policy, then tag types, then DryRun, then MaxResults.
func TestEC2Pipeline_CheckOrder(t *testing.T) {
	cases := []struct {
		name string
		deny bool
		body string
		code string
	}{
		{"policy denial beats every parameter error", true, "Action=CreateKeyPair&KeyName=k&DryRun=true&TagSpecification.1.ResourceType=instance", "UnauthorizedOperation"},
		{"tag type beats dry run", false, "Action=CreateKeyPair&KeyName=k&DryRun=true&TagSpecification.1.ResourceType=instance", "InvalidParameterValue"},
		{"dry run beats max results", false, "Action=DescribeInstances&DryRun=true&MaxResults=1", "DryRunOperation"},
		{"max results alone", false, "Action=DescribeInstances&MaxResults=1", "InvalidParameterValue"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stmts []iamdomain.Statement
			if !c.deny {
				stmts = append(stmts, allowEC2())
			}
			app := ec2Pipeline(t, nil, stmts...)
			got := probeEC2(t, app.Handler, c.body)
			assert.Equal(t, c.code, got.Code, got.Body)
		})
	}
}

// TestEC2Pipeline_Success pins the success envelope for a daemon-backed list
// and for the gateway-local actions.
func TestEC2Pipeline_Success(t *testing.T) {
	app := ec2Pipeline(t, nil, allowEC2())

	t.Run("daemon backed list", func(t *testing.T) {
		got := probeEC2(t, app.Handler, "Action=DescribeKeyPairs")
		require.Equal(t, 200, got.Status, got.Body)
		assert.Equal(t, "text/xml", got.ContentType)
		assert.Contains(t, got.Body, "<DescribeKeyPairsResponse")
		assert.Contains(t, got.Body, "<requestId>")
		assert.Equal(t, 1, app.Daemon.count("ec2.DescribeKeyPairs"))
	})

	t.Run("DescribeRegions needs no daemon", func(t *testing.T) {
		before := app.Daemon.count("ec2.DescribeRegions")
		got := probeEC2(t, app.Handler, "Action=DescribeRegions")
		require.Equal(t, 200, got.Status, got.Body)
		assert.Contains(t, got.Body, gwfix.Region)
		assert.Contains(t, got.Body, "https://registry.example", "endpoint is built from the registry host and port")
		assert.Equal(t, before, app.Daemon.count("ec2.DescribeRegions"))
	})

	t.Run("DescribeAvailabilityZones needs no daemon", func(t *testing.T) {
		got := probeEC2(t, app.Handler, "Action=DescribeAvailabilityZones")
		require.Equal(t, 200, got.Status, got.Body)
		assert.Contains(t, got.Body, gwfix.Region+"a")
	})

	t.Run("DescribeAccountAttributes looks up the default VPC", func(t *testing.T) {
		got := probeEC2(t, app.Handler, "Action=DescribeAccountAttributes")
		require.Equal(t, 200, got.Status, got.Body)
		assert.Contains(t, got.Body, "<DescribeAccountAttributesResponse")
		assert.Equal(t, 1, app.Daemon.count("ec2.DescribeVpcs"))
	})
}

// TestEC2Pipeline_IdempotentCreate pins ClientToken handling on a creating
// action: a replay returns the first result, changed parameters are refused,
// and no token means no deduplication.
func TestEC2Pipeline_IdempotentCreate(t *testing.T) {
	app := ec2Pipeline(t, nil, allowEC2())
	const subject = "ec2.CreateEgressOnlyInternetGateway"
	create := func(vpc, token string) ec2Outcome {
		body := "Action=CreateEgressOnlyInternetGateway&VpcId=" + vpc
		if token != "" {
			body += "&ClientToken=" + token
		}
		return probeEC2(t, app.Handler, body)
	}

	first := create("vpc-1", "tok-1")
	require.Equal(t, 200, first.Status, first.Body)
	assert.Contains(t, first.Body, "eigw-0001")

	replay := create("vpc-1", "tok-1")
	require.Equal(t, 200, replay.Status, replay.Body)
	assert.Contains(t, replay.Body, "eigw-0001", "same token returns the first result")
	assert.Equal(t, 1, app.Daemon.count(subject), "the replay does not reach the daemon")

	mismatch := create("vpc-2", "tok-1")
	assert.Equal(t, "IdempotentParameterMismatch", mismatch.Code, mismatch.Body)
	assert.Equal(t, 400, mismatch.Status)
	assert.Equal(t, 1, app.Daemon.count(subject))

	for range 2 {
		require.Equal(t, 200, create("vpc-1", "").Status)
	}
	assert.Equal(t, 3, app.Daemon.count(subject), "without a token each call creates")
}

// TestEC2Pipeline_PassRole pins the gateway iam:PassRole gate on an action
// that hands an instance profile over: a denied role is UnauthorizedOperation,
// a permitted one reaches the daemon.
func TestEC2Pipeline_PassRole(t *testing.T) {
	const body = "Action=AssociateIamInstanceProfile&InstanceId=i-dev&IamInstanceProfile.Name=web"
	passRole := statement("Allow", "iam:PassRole", "*")
	passRole.Condition = map[string]map[string]iamdomain.ConditionValue{
		iampolicy.OpStringEquals: {iampolicy.KeyPassedToService: {"ec2.amazonaws.com"}},
	}

	t.Run("permitted role reaches the daemon", func(t *testing.T) {
		app := ec2Pipeline(t, nil, allowEC2(), passRole)
		got := probeEC2(t, app.Handler, body)
		assert.Less(t, got.Status, 500, got.Body)
		assert.NotEqual(t, "UnauthorizedOperation", got.Code, got.Body)
		assert.Equal(t, 1, app.Daemon.count(ec2v1.InstanceCommandSubject("i-dev")))
	})

	t.Run("no PassRole", func(t *testing.T) {
		app := ec2Pipeline(t, nil, allowEC2())
		got := probeEC2(t, app.Handler, body)
		assert.Equal(t, 403, got.Status, got.Body)
		assert.Equal(t, "UnauthorizedOperation", got.Code, got.Body)
		assert.Zero(t, app.Daemon.count(ec2v1.InstanceCommandSubject("i-dev")))
	})

	t.Run("PassRole for another service", func(t *testing.T) {
		other := statement("Allow", "iam:PassRole", "*")
		other.Condition = map[string]map[string]iamdomain.ConditionValue{
			iampolicy.OpStringEquals: {iampolicy.KeyPassedToService: {"ecs-tasks.amazonaws.com"}},
		}
		app := ec2Pipeline(t, nil, allowEC2(), other)
		got := probeEC2(t, app.Handler, body)
		assert.Equal(t, 403, got.Status, got.Body)
		assert.Equal(t, "UnauthorizedOperation", got.Code, got.Body)
	})
}

// TestEC2Pipeline_ImportKeyPairPadding pins the base64 padding workaround: a
// doubly encoded trailing "==" is decoded before the key reaches the daemon,
// and a single trailing "=" is not.
func TestEC2Pipeline_ImportKeyPairPadding(t *testing.T) {
	forwarded := func(t *testing.T, material string) string {
		t.Helper()
		app := ec2Pipeline(t, nil, allowEC2())
		body := "Action=ImportKeyPair&KeyName=k&PublicKeyMaterial=" + url.QueryEscape(material)
		got := probeEC2(t, app.Handler, body)
		require.Equal(t, 200, got.Status, got.Body)
		var in ec2.ImportKeyPairInput
		require.NoError(t, json.NewDecoder(bytes.NewReader(app.Daemon.body("ec2.ImportKeyPair"))).Decode(&in))
		return string(in.PublicKeyMaterial)
	}

	t.Run("double padding", func(t *testing.T) {
		assert.Equal(t, "ssh-ed25519 AAAA==", forwarded(t, "ssh-ed25519 AAAA%3D%3D"))
	})
	t.Run("single padding is left encoded", func(t *testing.T) {
		assert.Equal(t, "ssh-ed25519 AAAA%3D", forwarded(t, "ssh-ed25519 AAAA%3D"))
	})
	t.Run("already decoded", func(t *testing.T) {
		assert.Equal(t, "ssh-ed25519 AAAA==", forwarded(t, "ssh-ed25519 AAAA=="))
	})
}

const runInstancesBase = "Action=RunInstances&InstanceType=t3.micro&MinCount=1&MaxCount=1"

// TestEC2Pipeline_RunInstancesLaunchTemplateAuthorizedAfterExpansion pins that
// a launch template is expanded before the policy gate, so a resource the
// template supplies is authorized and a Deny on it fires.
func TestEC2Pipeline_RunInstancesLaunchTemplateAuthorizedAfterExpansion(t *testing.T) {
	const subject = "ec2.DescribeLaunchTemplateVersions"
	template := func(image string) string {
		return `{"LaunchTemplateVersions":[{"LaunchTemplateData":{"ImageId":"` + image + `"}}]}`
	}
	const body = "Action=RunInstances&InstanceType=t3.micro&MinCount=1&MaxCount=1&LaunchTemplate.LaunchTemplateName=lt"
	deny := statement("Deny", "ec2:RunInstances", "arn:aws:ec2:*:*:image/ami-fenced")

	t.Run("template image is denied", func(t *testing.T) {
		app := ec2Pipeline(t, nil, allowEC2(), deny)
		app.Daemon.reply(subject, template("ami-fenced"))
		got := probeEC2(t, app.Handler, body)
		assert.Equal(t, 403, got.Status, got.Body)
		assert.Equal(t, "UnauthorizedOperation", got.Code, got.Body)
		assert.Equal(t, 1, app.Daemon.count(subject))
		assert.Zero(t, app.Daemon.count("ec2.RunInstances"))
	})

	t.Run("template image not fenced passes the gate", func(t *testing.T) {
		app := ec2Pipeline(t, nil, allowEC2(), deny)
		app.Daemon.reply(subject, template("ami-ok"))
		got := probeEC2(t, app.Handler, body)
		assert.NotEqual(t, "UnauthorizedOperation", got.Code, got.Body)
	})

	t.Run("unknown template", func(t *testing.T) {
		app := ec2Pipeline(t, nil, allowEC2())
		app.Daemon.reply(subject, `{"LaunchTemplateVersions":[]}`)
		got := probeEC2(t, app.Handler, body)
		assert.Equal(t, "InvalidLaunchTemplateId.VersionNotFound", got.Code, got.Body)
	})
}

// TestEC2Pipeline_RunInstancesInstanceProfile pins the current order of the
// profile checks on RunInstances: the profile is resolved first, so an unknown
// one is refused before iam:PassRole is consulted, and a resolved one is then
// subject to PassRole on its role. This is the baseline for any parity change.
func TestEC2Pipeline_RunInstancesInstanceProfile(t *testing.T) {
	const body = runInstancesBase + "&ImageId=ami-1&IamInstanceProfile.Name=web"

	t.Run("unknown profile is refused before PassRole", func(t *testing.T) {
		app := ec2Pipeline(t, func(e *ec2PipelineEnv) { e.ResolveErr = errors.New(awserrors.ErrorIAMNoSuchEntity) }, allowEC2())
		got := probeEC2(t, app.Handler, body)
		assert.Equal(t, "InvalidIamInstanceProfile.NotFound", got.Code, got.Body)
		assert.Equal(t, 404, got.Status)
	})

	t.Run("resolved profile without PassRole", func(t *testing.T) {
		app := ec2Pipeline(t, nil, allowEC2())
		got := probeEC2(t, app.Handler, body)
		assert.Equal(t, 403, got.Status, got.Body)
		assert.Equal(t, "UnauthorizedOperation", got.Code, got.Body)
	})

	t.Run("other resolution failure is returned as is", func(t *testing.T) {
		app := ec2Pipeline(t, func(e *ec2PipelineEnv) { e.ResolveErr = errors.New(awserrors.ErrorServerInternal) }, allowEC2())
		got := probeEC2(t, app.Handler, body)
		assert.Equal(t, "ServerInternal", got.Code, got.Body)
	})
}
