package pipeline

//test:in-package — this characterization reads unexported request-context keys.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	rdsawsapi "github.com/mulgadc/spinifex/spinifex/domains/rds/awsapi"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// The tests in this file pin the caller facts the SigV4 door hands a registered
// service for every principal kind: the dispatch.Actor, the request-context
// values the legacy handlers read, and what the RDS and EKS agent binding
// derives from them. They describe current behaviour, not the target design.

const actorProbeService = "actorprobe"

// actorObservation is everything a handler can learn about its caller today.
type actorObservation struct {
	Actor dispatch.Actor

	Identity          string
	PrincipalType     string
	AssumedRoleARN    string
	AssumedRoleID     string
	UnderlyingRoleARN string
	UserID            string
	UserARN           string

	RDSAccountID, RDSPrincipalType, RDSSessionName, RDSRoleName string
	EKSAccountID, EKSPrincipalType, EKSSessionName, EKSRoleName string
	EKSPrincipalARN                                             string
}

// actorProbeGateway serves env's services with a registered service that
// records what its handler can see of the verified caller.
func actorProbeGateway(t *testing.T, e *cbEnv) (http.Handler, *[]actorObservation) {
	t.Helper()
	var seen []actorObservation
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(dispatch.Registration{
		Service:       actorProbeService,
		ResolveAction: func(*http.Request) string { return "Probe" },
		Errors:        dispatch.ErrorEnvelopeXML,
		Inventory:     dispatch.Inventory{Registered: []string{"Probe"}},
		Dispatch: func(w http.ResponseWriter, inv dispatch.Invocation) error {
			actor, err := inv.Actor()
			if err != nil {
				return err
			}
			r := inv.Request
			rds := rdsawsapi.CallerFromActor(actor)
			eks := eksawsapi.CallerFromActor(actor)
			seen = append(seen, actorObservation{
				Actor:             actor,
				Identity:          callerOf(r).Identity,
				PrincipalType:     callerOf(r).PrincipalType,
				AssumedRoleARN:    callerOf(r).AssumedRoleARN,
				AssumedRoleID:     callerOf(r).AssumedRoleID,
				UnderlyingRoleARN: callerOf(r).UnderlyingRoleARN,
				UserID:            callerOf(r).UserID,
				UserARN:           callerOf(r).UserARN,
				RDSAccountID:      rds.AccountID, RDSPrincipalType: rds.PrincipalType,
				RDSSessionName: rds.SessionName, RDSRoleName: rds.RoleName,
				EKSAccountID: eks.AccountID, EKSPrincipalType: eks.PrincipalType,
				EKSSessionName: eks.SessionName, EKSRoleName: eks.RoleName,
				EKSPrincipalARN: actor.CallerARN,
			})
			w.WriteHeader(http.StatusOK)
			return nil
		},
	}))
	gw := gwfix.WithDeps(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
	}, gwfix.Deps{NATS: e.nc, Nodes: 1, IAM: e.counter, STS: e.sts})
	withRealAuth(gw)
	gw.PreAuth = append(gw.PreAuth, stsawsapi.NewPreAuth(stsawsapi.Deps{STS: gwfix.DepsOf(gw).STS, IAM: gwfix.DepsOf(gw).IAM}))
	require.NoError(t, b.Register(stsRegistration(gw)))
	gw.Services = b.Build()
	return gw.SetupRoutes(), &seen
}

// probe sends one signed request as c and returns what the handler observed.
func probe(t *testing.T, e *cbEnv, h http.Handler, seen *[]actorObservation, c cbCreds) actorObservation {
	t.Helper()
	before := len(*seen)
	rec := e.request(t, h, c, actorProbeService, cbForm("Probe"))
	require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Len(t, *seen, before+1, "handler ran once")
	return (*seen)[before]
}

// cbWebIdentitySession mints an AssumeRoleWithWebIdentity session for role
// "irsa" and session name "pod" in the env's tenant account.
func cbWebIdentitySession(t *testing.T, e *cbEnv) (cbCreds, string) {
	t.Helper()
	issuer := testDiscIssuer
	iamKV, err := kvutil.GetOrCreateBucket(t.Context(), e.js,
		iamdomain.IAMAccountBucketName(e.accountID), iamdomain.KVBucketIAMAccountVersion)
	require.NoError(t, err)
	_, err = iamKV.Put(t.Context(), iamdomain.OIDCProviderKey(issuer), []byte(`{"registered":true}`))
	require.NoError(t, err)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	point, err := priv.PublicKey.Bytes()
	require.NoError(t, err)
	jwks, err := json.Marshal(stsdomain.JWKS{Keys: []stsdomain.JWK{{
		Kty: "EC", Kid: "k1", Alg: "ES256", Use: "sig", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(point[1:33]),
		Y: base64.RawURLEncoding.EncodeToString(point[33:65]),
	}}})
	require.NoError(t, err)
	eksKV, err := ekscluster.GetOrCreateAccountBucket(t.Context(), e.js, e.accountID)
	require.NoError(t, err)
	_, err = eksKV.Put(t.Context(), ekscluster.OIDCJWKSKey(testDiscCluster), jwks)
	require.NoError(t, err)

	federated := iamdomain.OIDCProviderARN(e.accountID, strings.TrimPrefix(issuer, "https://"))
	role, err := e.iam.CreateRole(e.accountID, &iam.CreateRoleInput{
		RoleName: aws.String("irsa"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"` +
			federated + `"},"Action":"sts:AssumeRoleWithWebIdentity"}]}`),
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": issuer, "sub": "system:serviceaccount:default:sa", "aud": []string{"sts.amazonaws.com"},
		"exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix(),
	})
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(priv)
	require.NoError(t, err)

	s := cbSessionFrom(t, e.request(t, e.h, cbCreds{}, "sts", cbForm("AssumeRoleWithWebIdentity",
		"RoleArn", aws.StringValue(role.Role.Arn), "RoleSessionName", "pod", "WebIdentityToken", signed)))
	return s, aws.StringValue(role.Role.RoleId)
}

// cbInstanceRoleSession mints the session IMDS serves an instance: role name
// in the global account, session name the instance ID.
func cbInstanceRoleSession(t *testing.T, e *cbEnv, roleName, instanceID string) (cbCreds, string) {
	t.Helper()
	_, err := iamdomain.EnsureSystemInstanceProfile(e.iam, awsidentifiers.GlobalAccountID,
		roleName, "probe-policy", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)
	require.NoError(t, err)
	roleARN := "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":role/" + roleName
	out, err := e.sts.AssumeRoleForInstance(awsidentifiers.GlobalAccountID, roleARN, instanceID, 3600)
	require.NoError(t, err)
	return cbCreds{
		akid:   aws.StringValue(out.Credentials.AccessKeyId),
		secret: aws.StringValue(out.Credentials.SecretAccessKey),
		token:  aws.StringValue(out.Credentials.SessionToken),
	}, roleARN
}

// userObservation is the expected observation for a user principal.
func userObservation(accountID, callerARN, akid, identity, userID, userARN, principalARN string) actorObservation {
	return actorObservation{
		Actor: dispatch.Actor{
			AccountID: accountID, CallerARN: callerARN, PrincipalType: "user", AccessKeyID: akid,
			PrincipalARN: principalARN, UserID: userID, UserName: identity,
		},
		Identity: identity, PrincipalType: "user", UserID: userID, UserARN: userARN,
		RDSAccountID: accountID, RDSPrincipalType: "user", RDSSessionName: identity,
		EKSAccountID: accountID, EKSPrincipalType: "user", EKSSessionName: identity,
		EKSPrincipalARN: principalARN,
	}
}

// roleObservation is the expected observation for an assumed-role session.
func roleObservation(accountID, roleName, roleID, session, akid string) actorObservation {
	sessionARN := "arn:aws:sts::" + accountID + ":assumed-role/" + roleName + "/" + session
	return actorObservation{
		Actor: dispatch.Actor{
			AccountID: accountID, CallerARN: sessionARN, PrincipalType: "assumed-role", AccessKeyID: akid,
			PrincipalARN: "arn:aws:iam::" + accountID + ":role/" + roleName, UserID: roleID + ":" + session, SessionName: session,
		},
		Identity: session, PrincipalType: "assumed-role",
		AssumedRoleARN: sessionARN, AssumedRoleID: roleID + ":" + session,
		UnderlyingRoleARN: "arn:aws:iam::" + accountID + ":role/" + roleName,
		UserID:            roleID + ":" + session,
		RDSAccountID:      accountID, RDSPrincipalType: "assumed-role", RDSSessionName: session, RDSRoleName: roleName,
		EKSAccountID: accountID, EKSPrincipalType: "assumed-role", EKSSessionName: session, EKSRoleName: roleName,
		EKSPrincipalARN: sessionARN,
	}
}

// Pins CURRENT behaviour: the Actor and request-context values each principal
// kind produces at the door. Notable current facts:
//   - the global root reports PrincipalType "user", not "root";
//   - a tenant user named root gets the account ID as aws:userid and no stored
//     user ARN, and shares the session name "root" with the global root;
//   - a user principal's identity is its user name, a role session's its session name.
func TestActorProjection_PrincipalKinds(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	h, seen := actorProbeGateway(t, e)
	acct := e.accountID
	global := awsidentifiers.GlobalAccountID

	roleARN, appRoleID := e.createAssumableRole(t, "app")
	rootUser, _ := e.newUser(t, "root")
	getSession := e.getSessionToken(t, e.alice)
	assumed := e.assumeRole(t, roleARN, "s1")
	webID, irsaRoleID := cbWebIdentitySession(t, e)
	imds, _ := cbInstanceRoleSession(t, e, rdsinstance.InstanceRoleName, "i-0123456789abcdef0")
	rdsRole, err := e.iam.GetRole(global, &iam.GetRoleInput{RoleName: aws.String(rdsinstance.InstanceRoleName)})
	require.NoError(t, err)

	aliceARN := "arn:aws:iam::" + acct + ":user/alice"
	rootCreds := cbCreds{akid: cbRootAKID, secret: cbRootSecret}
	globalRootARN := "arn:aws:iam::" + global + ":root"

	for _, tc := range []struct {
		name  string
		creds cbCreds
		want  actorObservation
	}{
		{"global_root", rootCreds,
			userObservation(global, globalRootARN, cbRootAKID, "root", global, "", globalRootARN)},
		{"tenant_user_named_root", rootUser,
			userObservation(acct, "arn:aws:iam::"+acct+":user/root", rootUser.akid, "root", acct, "", "arn:aws:iam::"+acct+":user/root")},
		{"iam_user", e.alice,
			userObservation(acct, aliceARN, e.alice.akid, "alice", e.aliceID, aliceARN, aliceARN)},
		{"get_session_token", getSession,
			userObservation(acct, aliceARN, getSession.akid, "alice", e.aliceID, aliceARN, aliceARN)},
		{"assume_role", assumed, roleObservation(acct, "app", appRoleID, "s1", assumed.akid)},
		{"assume_role_with_web_identity", webID, roleObservation(acct, "irsa", irsaRoleID, "pod", webID.akid)},
		{"imds_instance_role", imds,
			roleObservation(global, rdsinstance.InstanceRoleName, aws.StringValue(rdsRole.Role.RoleId), "i-0123456789abcdef0", imds.akid)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, probe(t, e, h, seen, tc.creds))
		})
	}
}

// Pins CURRENT behaviour: RDS and EKS derive the same caller from the same
// context, and the role name comes from the underlying role ARN, never the
// session name the caller chose at AssumeRole time.
func TestActorProjection_RDSAndEKSCallersAgree(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	h, seen := actorProbeGateway(t, e)

	roleARN, _ := e.createAssumableRole(t, "app")
	imds, _ := cbInstanceRoleSession(t, e, rdsinstance.InstanceRoleName, "i-0123456789abcdef0")
	cpAgent, _ := cbInstanceRoleSession(t, e, ekscluster.CPInstanceRoleName, "i-0fedcba9876543210")

	for name, c := range map[string]cbCreds{
		"global_root": {akid: cbRootAKID, secret: cbRootSecret},
		"iam_user":    e.alice,
		"assume_role": e.assumeRole(t, roleARN, "s1"),
		"rds_agent":   imds,
		"eks_cp":      cpAgent,
	} {
		t.Run(name, func(t *testing.T) {
			o := probe(t, e, h, seen, c)
			assert.Equal(t, o.RDSAccountID, o.EKSAccountID)
			assert.Equal(t, o.RDSPrincipalType, o.EKSPrincipalType)
			assert.Equal(t, o.RDSSessionName, o.EKSSessionName)
			assert.Equal(t, o.RDSRoleName, o.EKSRoleName)
		})
	}

	t.Run("agent_role_names", func(t *testing.T) {
		o := probe(t, e, h, seen, imds)
		assert.Equal(t, rdsinstance.InstanceRoleName, o.RDSRoleName)
		o = probe(t, e, h, seen, cpAgent)
		assert.Equal(t, ekscluster.CPInstanceRoleName, o.EKSRoleName)
		assert.Equal(t, awsidentifiers.GlobalAccountID, o.EKSAccountID)
	})

	// A tenant session named after an agent role does not become that role.
	for _, name := range []string{rdsinstance.InstanceRoleName, ekscluster.CPInstanceRoleName} {
		t.Run("session_named_"+name, func(t *testing.T) {
			o := probe(t, e, h, seen, e.assumeRole(t, roleARN, name))
			assert.Equal(t, name, o.RDSSessionName)
			assert.Equal(t, "app", o.RDSRoleName)
			assert.Equal(t, "app", o.EKSRoleName)
			assert.NotEqual(t, awsidentifiers.GlobalAccountID, o.RDSAccountID)
		})
	}

	// A user principal has a session name (its user name) but never a role name.
	t.Run("user_has_no_role_name", func(t *testing.T) {
		o := probe(t, e, h, seen, e.alice)
		assert.Equal(t, "alice", o.RDSSessionName)
		assert.Empty(t, o.RDSRoleName)
		assert.Empty(t, o.EKSRoleName)
	})
}

// Pins CURRENT behaviour: the RDS and EKS agent roles trust only
// ec2.amazonaws.com, so no signed caller can assume them over HTTPS, whatever
// session name it picks, and a same-named role in another account never
// passes the global-account gate.
func TestActorProjection_AgentRolesNotAssumableOverHTTPS(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	h, seen := actorProbeGateway(t, e)
	global := awsidentifiers.GlobalAccountID
	// Alice may call AssumeRole on anything, so each denial below comes from the role trust policy.
	e.putUserPolicy(t, "alice", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sts:AssumeRole","Resource":"*"}]}`)

	for _, role := range []string{rdsinstance.InstanceRoleName, ekscluster.CPInstanceRoleName} {
		_, _ = cbInstanceRoleSession(t, e, role, "i-0123456789abcdef0")
		target := "arn:aws:iam::" + global + ":role/" + role
		for name, c := range map[string]cbCreds{
			"tenant_user": e.alice,
			"global_root": {akid: cbRootAKID, secret: cbRootSecret},
		} {
			t.Run(role+"/"+name, func(t *testing.T) {
				rec := e.request(t, e.h, c, "sts", cbForm("AssumeRole",
					"RoleArn", target, "RoleSessionName", "i-0123456789abcdef0"))
				assert.Equal(t, cbRegistry(awserrors.ErrorAccessDenied).code, cbOutcomeOf(rec).code)
				assert.Equal(t, http.StatusForbidden, rec.Code)
			})
		}
	}

	// A tenant that creates its own role with an agent role's name carries the
	// name but not the global account.
	_, err := e.iam.CreateRole(e.accountID, &iam.CreateRoleInput{
		RoleName: aws.String(rdsinstance.InstanceRoleName),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` +
			e.accountID + `:user/alice"},"Action":"sts:AssumeRole"}]}`),
	})
	require.NoError(t, err)
	o := probe(t, e, h, seen, e.assumeRole(t,
		"arn:aws:iam::"+e.accountID+":role/"+rdsinstance.InstanceRoleName, "mine"))
	assert.Equal(t, rdsinstance.InstanceRoleName, o.RDSRoleName)
	assert.Equal(t, e.accountID, o.RDSAccountID)
	assert.NotEqual(t, global, o.RDSAccountID)
}

const (
	testDiscAccount = "000000000001"
	testDiscCluster = "h1"
	testDiscIssuer  = "https://10.0.0.1:9999/oidc/eks/ap-southeast-2/000000000001/h1"
)
