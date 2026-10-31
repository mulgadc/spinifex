package awsapi

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	ecsdomain "github.com/mulgadc/spinifex/spinifex/domains/ecs"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccount = "123456789012"
	ownRole     = "arn:aws:iam::123456789012:role/task-role"
)

// recordingAuth records the resources PassRole was asked about and denies
// them when deny is set.
type recordingAuth struct {
	deny      bool
	resources []string
}

func (a *recordingAuth) authorize(_, _ string, resources []string, _ iampolicy.ConditionKeys) error {
	a.resources = append(a.resources, resources...)
	if a.deny {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	return nil
}

// mapRoles is a Resolver over a fixed set of existing role ARNs.
type mapRoles map[string]bool

func (m mapRoles) ResolveRole(accountID, roleARN string) (passrole.Role, error) {
	role, err := accountRoles{}.ResolveRole(accountID, roleARN)
	if err != nil {
		return passrole.Role{}, err
	}
	if !m[roleARN] {
		return passrole.Role{}, passrole.ErrNotFound
	}
	return role, nil
}

func errCode(err error) string {
	code, _ := awserrors.ResolveErrorCode(err)
	return code
}

// The ECS expectations below are the 2026-10-30 AWS capture in
// scripts/aws-compliance/reports/evidence/ecs-eks-passrole-aws-baseline-2026-10-30.tar.xz.
func TestRoleChecker_RegisterTaskDefinition_AwsCapture(t *testing.T) {
	tests := []struct {
		name         string
		role         string
		deny         bool
		wantCode     string
		wantAuthed   []string
		wantErrorMsg string
	}{
		{name: "foreign role is refused before PassRole, even when PassRole is denied", role: "arn:aws:iam::999999999999:role/r", deny: true, wantCode: awserrors.ErrorECSClientException, wantErrorMsg: "Role is not valid"},
		{name: "foreign role without PassRole", role: "arn:aws:iam::999999999999:role/r", wantCode: awserrors.ErrorECSClientException, wantErrorMsg: "Role is not valid"},
		{name: "not an ARN is authorized as written then accepted", role: "not-an-arn", wantAuthed: []string{"not-an-arn"}},
		{name: "not an ARN without PassRole is denied", role: "not-an-arn", deny: true, wantCode: awserrors.ErrorAccessDenied, wantAuthed: []string{"not-an-arn"}},
		{name: "nonexistent role is authorized then accepted", role: "arn:aws:iam::123456789012:role/missing", wantAuthed: []string{"arn:aws:iam::123456789012:role/missing"}},
		{name: "nonexistent role without PassRole is denied, not reported missing", role: "arn:aws:iam::123456789012:role/missing", deny: true, wantCode: awserrors.ErrorAccessDenied, wantAuthed: []string{"arn:aws:iam::123456789012:role/missing"}},
		{name: "valid role is authorized then accepted", role: ownRole, wantAuthed: []string{ownRole}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &recordingAuth{deny: tt.deny}
			check := roleChecker(accountRoles{}, auth.authorize, testAccount, nil)

			err := checkTaskDefinitionRoles(check, aws.String(tt.role))

			if tt.wantCode == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.wantCode, errCode(err))
			}
			if tt.wantErrorMsg != "" {
				assert.Contains(t, err.Error(), tt.wantErrorMsg)
			}
			assert.Equal(t, tt.wantAuthed, auth.resources)
		})
	}
}

// fakeRunTaskECS answers the two calls RunTask makes.
type fakeRunTaskECS struct {
	ecsdomain.ECSService

	launched bool
}

func (f *fakeRunTaskECS) RunTask(context.Context, *ecs.RunTaskInput, string) (*ecs.RunTaskOutput, error) {
	f.launched = true
	return &ecs.RunTaskOutput{}, nil
}

func TestRunTask_OverrideRole_AwsCapture(t *testing.T) {
	roles := mapRoles{ownRole: true}
	tests := []struct {
		name     string
		role     string
		deny     bool
		wantCode string
		wantMsg  string
		launched bool
	}{
		{name: "foreign override", role: "arn:aws:iam::999999999999:role/r", wantCode: awserrors.ErrorECSClientException, wantMsg: "Role is not valid"},
		{name: "nonexistent override reports ECS could not assume it", role: "arn:aws:iam::123456789012:role/missing", wantCode: awserrors.ErrorECSClientException, wantMsg: "ECS was unable to assume the role 'arn:aws:iam::123456789012:role/missing'"},
		{name: "nonexistent override without PassRole is denied", role: "arn:aws:iam::123456789012:role/missing", deny: true, wantCode: awserrors.ErrorAccessDenied},
		{name: "invented path on a real role", role: "arn:aws:iam::123456789012:role/invented/task-role", wantCode: awserrors.ErrorECSClientException, wantMsg: "ECS was unable to assume the role"},
		{name: "valid override launches", role: ownRole, launched: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &recordingAuth{deny: tt.deny}
			ctx := withOverrideRoleCheck(t.Context(), roleChecker(roles, auth.authorize, testAccount, unableToAssume))
			svc := &fakeRunTaskECS{}
			input := &ecs.RunTaskInput{Overrides: &ecs.TaskOverride{TaskRoleArn: aws.String(tt.role)}}

			_, err := runTask(ctx, svc, testAccount, input, nil)

			if tt.wantCode == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.wantCode, errCode(err))
			}
			if tt.wantMsg != "" {
				assert.Contains(t, err.Error(), tt.wantMsg)
			}
			assert.Equal(t, tt.launched, svc.launched)
		})
	}
}
