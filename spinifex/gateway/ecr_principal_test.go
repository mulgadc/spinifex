package gateway

import (
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ecrMockIAMService implements iamdomain.IAMService for ECR principal
// rehydration tests. Only the methods resolveECRPrincipal calls are wired;
// any other method nil-panics on the embedded nil interface, surfacing an
// unexpected call rather than silently returning a zero value.
type ecrMockIAMService struct {
	iamdomain.IAMService

	accessKeys   map[string]*iamdomain.AccessKey
	users        map[string]*iam.User
	roles        map[string]*iam.Role
	accounts     map[string]*iamdomain.Account
	userPolicies map[string][]iamdomain.PolicyDocument
	rolePolicies map[string][]iamdomain.PolicyDocument

	// genericErr, when set, is returned by LookupAccessKey/GetUser/GetRole/
	// GetAccount/GetUserPolicies/GetRolePolicies instead of the usual
	// not-found error, simulating an IAM backend outage rather than a
	// missing record.
	genericErr error

	// userPolicyErr fails only the policy read, so a test can let the principal
	// rehydrate and then break authorization.
	userPolicyErr error
}

func newECRMockIAMService() *ecrMockIAMService {
	return &ecrMockIAMService{
		accessKeys:   map[string]*iamdomain.AccessKey{},
		users:        map[string]*iam.User{},
		roles:        map[string]*iam.Role{},
		accounts:     map[string]*iamdomain.Account{},
		userPolicies: map[string][]iamdomain.PolicyDocument{},
		rolePolicies: map[string][]iamdomain.PolicyDocument{},
	}
}

func (m *ecrMockIAMService) LookupAccessKey(accessKeyID string) (*iamdomain.AccessKey, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	ak, ok := m.accessKeys[accessKeyID]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return ak, nil
}

func (m *ecrMockIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	u, ok := m.users[accountID+"|"+aws.StringValue(input.UserName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetUserOutput{User: u}, nil
}

func (m *ecrMockIAMService) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	r, ok := m.roles[accountID+"|"+aws.StringValue(input.RoleName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: r}, nil
}

func (m *ecrMockIAMService) GetAccount(accountID string) (*iamdomain.Account, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	a, ok := m.accounts[accountID]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return a, nil
}

func (m *ecrMockIAMService) GetUserPolicies(accountID, userName string) ([]iamdomain.PolicyDocument, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	if m.userPolicyErr != nil {
		return nil, m.userPolicyErr
	}
	return m.userPolicies[accountID+"|"+userName], nil
}

func (m *ecrMockIAMService) GetRolePolicies(accountID, roleName string) ([]iamdomain.PolicyDocument, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	return m.rolePolicies[accountID+"|"+roleName], nil
}

// ecrMockSTSService implements stsdomain.STSService for ECR principal
// rehydration tests. Only LookupSessionCredential and VerifySessionPrincipal
// are wired.
type ecrMockSTSService struct {
	stsdomain.STSService

	sessions map[string]*stsdomain.SessionCredential

	// principalErr is the continuity verdict (or dependency fault) the check
	// returns. Detection itself is exercised against real IAM/STS services in
	// domains/sts; what these tests pin is how this door maps the answer.
	principalErr error

	// liveUserARN is the stored ARN the continuity read returns for a user session.
	liveUserARN string
}

func newECRMockSTSService() *ecrMockSTSService {
	return &ecrMockSTSService{sessions: map[string]*stsdomain.SessionCredential{}}
}

func (m *ecrMockSTSService) LookupSessionCredential(accessKeyID string) (*stsdomain.SessionCredential, error) {
	return m.sessions[accessKeyID], nil
}

func (m *ecrMockSTSService) VerifySessionPrincipal(cred *stsdomain.SessionCredential) (*stsdomain.SessionPrincipal, error) {
	if m.principalErr != nil {
		return nil, m.principalErr
	}
	live, err := stubSessionPrincipal(cred)
	if err == nil && cred.PrincipalType == principalTypeUser {
		live.UserARN = m.liveUserARN
	}
	return live, err
}

const (
	ecrPrincipalTestAccount = "000000000042"
	ecrPrincipalTestAKID    = "AKIATESTLONGLIVED0001"
	ecrPrincipalTestASID    = "ASIATESTSESSION000001"
)

func seedECRTestUser(iamSvc *ecrMockIAMService, accountID, userName, akid string) {
	iamSvc.accessKeys[akid] = &iamdomain.AccessKey{
		AccessKeyID: akid,
		UserName:    userName,
		AccountID:   accountID,
		Status:      iamdomain.AccessKeyStatusActive,
	}
	iamSvc.users[accountID+"|"+userName] = &iam.User{
		UserName: aws.String(userName),
		UserId:   aws.String("AIDA" + strings.ToUpper(userName)),
		Arn:      aws.String("arn:aws:iam::" + accountID + ":user/" + userName),
	}
	iamSvc.accounts[accountID] = &iamdomain.Account{AccountID: accountID, Status: iamdomain.AccountStatusActive}
}

const ecrPathedUserARN = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/eng/dev"

func seedECRSessionUser(iamSvc *ecrMockIAMService, stsSvc *ecrMockSTSService, accountID, userName, asid string, expiresAt time.Time) {
	iamSvc.users[accountID+"|"+userName] = &iam.User{
		UserName: aws.String(userName),
		UserId:   aws.String("AIDA" + strings.ToUpper(userName)),
		Arn:      aws.String("arn:aws:iam::" + accountID + ":user/" + userName),
	}
	iamSvc.accounts[accountID] = &iamdomain.Account{AccountID: accountID, Status: iamdomain.AccountStatusActive}
	stsSvc.sessions[asid] = &stsdomain.SessionCredential{
		AccessKeyID:   asid,
		AccountID:     accountID,
		PrincipalType: principalTypeUser,
		SessionName:   userName,
		UserID:        "AIDA" + strings.ToUpper(userName),
		ExpiresAt:     expiresAt,
	}
}

func seedECRAssumedRole(iamSvc *ecrMockIAMService, stsSvc *ecrMockSTSService, accountID, roleName, roleID, asid, sessionName string, expiresAt time.Time) string {
	roleARN := "arn:aws:iam::" + accountID + ":role/" + roleName
	iamSvc.roles[accountID+"|"+roleName] = &iam.Role{
		RoleName: aws.String(roleName),
		Arn:      aws.String(roleARN),
		RoleId:   aws.String(roleID),
	}
	iamSvc.accounts[accountID] = &iamdomain.Account{AccountID: accountID, Status: iamdomain.AccountStatusActive}
	// AssumeRole session ARNs use the "assumed-role" resource type; the
	// underlying role (resolved via GetRole) stays a role/ ARN, matching the
	// production shape.
	assumedRoleARN := "arn:aws:sts::" + accountID + ":assumed-role/" + roleName + "/" + sessionName
	stsSvc.sessions[asid] = &stsdomain.SessionCredential{
		AccessKeyID:       asid,
		AccountID:         accountID,
		PrincipalType:     principalTypeAssumedRole,
		SessionName:       sessionName,
		AssumedRoleARN:    assumedRoleARN,
		UnderlyingRoleARN: roleARN,
		RoleID:            roleID,
		ExpiresAt:         expiresAt,
	}
	return assumedRoleARN
}
