package gateway

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type iamExamplesFile struct {
	Examples map[string][]iamExample `json:"examples"`
}

type iamExample struct {
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Title  string          `json:"title"`
}

func TestIAMExamplesContract(t *testing.T) {
	examples := loadIAMExamples(t)
	covered := 0

	for action, actionExamples := range examples.Examples {
		if _, ok := iamActions[action]; !ok {
			continue
		}

		for i, example := range actionExamples {
			name := action
			if len(actionExamples) > 1 {
				name = fmt.Sprintf("%s/%d", action, i)
			}

			t.Run(name, func(t *testing.T) {
				svc := &echoIAMService{}
				server := httptest.NewServer(setupIAMRequestHandler(svc))
				defer server.Close()

				client := iam.New(session.Must(session.NewSession(&aws.Config{
					Credentials: credentials.NewStaticCredentials("test", "test", ""),
					DisableSSL:  aws.Bool(true),
					Endpoint:    aws.String(server.URL),
					MaxRetries:  aws.Int(0),
					Region:      aws.String("us-east-1"),
				})))

				method := reflect.ValueOf(client).MethodByName(action)
				require.True(t, method.IsValid(), "aws-sdk-go IAM client has no %s method", action)
				require.Equal(t, 1, method.Type().NumIn(), "%s client method input arity", action)
				require.Equal(t, 2, method.Type().NumOut(), "%s client method output arity", action)

				inputType := method.Type().In(0)
				outputType := method.Type().Out(0)
				require.Equal(t, reflect.Pointer, inputType.Kind(), "%s input type", action)
				require.Equal(t, reflect.Pointer, outputType.Kind(), "%s output type", action)

				wantInput := reflect.New(inputType.Elem())
				unmarshalIAMExample(t, action, "input", example.Input, wantInput.Interface())

				wantOutput := reflect.New(outputType.Elem())
				unmarshalIAMExample(t, action, "output", example.Output, wantOutput.Interface())
				svc.wantOutput = wantOutput.Interface()

				out := method.Call([]reflect.Value{wantInput})
				if errValue := out[1]; !errValue.IsNil() {
					t.Fatalf("%s SDK call failed: %v", action, errValue.Interface())
				}

				normalizeEmptySlices(wantOutput)
				normalizeEmptySlices(out[0])
				assert.Equal(t, "000000000000", svc.gotAccountID, "%s account ID", action)
				assert.Equal(t, wantInput.Interface(), svc.gotInput, "%s parsed input", action)
				assert.Equal(t, wantOutput.Interface(), out[0].Interface(), "%s decoded output", action)
			})
			covered++
		}
	}

	assert.Equal(t, 33, covered, "covered IAM examples")
}

func loadIAMExamples(t *testing.T) iamExamplesFile {
	t.Helper()

	data, err := os.ReadFile("testdata/aws/examples-1.json")
	require.NoError(t, err)

	var examples iamExamplesFile
	require.NoError(t, json.Unmarshal(data, &examples))
	require.NotEmpty(t, examples.Examples)
	return examples
}

func unmarshalIAMExample(t *testing.T, action, field string, data json.RawMessage, target any) {
	t.Helper()

	if len(data) == 0 || string(data) == "null" {
		return
	}
	require.NoError(t, json.Unmarshal(data, target), "%s example %s", action, field)
}

func normalizeEmptySlices(v reflect.Value) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Interface && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		normalizeEmptySlices(v.Elem())
		return
	}
	if v.Kind() != reflect.Struct {
		return
	}

	for _, field := range v.Fields() {
		field := field
		switch field.Kind() {
		case reflect.Slice:
			if field.Len() == 0 && field.CanSet() {
				field.Set(reflect.Zero(field.Type()))
			}
			for j := 0; j < field.Len(); j++ {
				normalizeEmptySlices(field.Index(j))
			}
		case reflect.Pointer, reflect.Struct, reflect.Interface:
			normalizeEmptySlices(field)
		}
	}
}

type echoIAMService struct {
	flexMockIAMService

	wantOutput   any
	gotAccountID string
	gotInput     any
}

func echoIAMCall[In any, Out any](svc *echoIAMService, accountID string, input *In) (*Out, error) {
	svc.gotAccountID = accountID
	svc.gotInput = input

	if svc.wantOutput == nil {
		return new(Out), nil
	}
	output, ok := svc.wantOutput.(*Out)
	if !ok {
		return nil, fmt.Errorf("echo IAM output type %T is not %T", svc.wantOutput, new(Out))
	}
	return output, nil
}

func (svc *echoIAMService) AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return echoIAMCall[iam.AddRoleToInstanceProfileInput, iam.AddRoleToInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) AddUserToGroup(accountID string, input *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	return echoIAMCall[iam.AddUserToGroupInput, iam.AddUserToGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachGroupPolicy(accountID string, input *iam.AttachGroupPolicyInput) (*iam.AttachGroupPolicyOutput, error) {
	return echoIAMCall[iam.AttachGroupPolicyInput, iam.AttachGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return echoIAMCall[iam.AttachRolePolicyInput, iam.AttachRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachUserPolicy(accountID string, input *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	return echoIAMCall[iam.AttachUserPolicyInput, iam.AttachUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	return echoIAMCall[iam.CreateAccessKeyInput, iam.CreateAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateGroup(accountID string, input *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	return echoIAMCall[iam.CreateGroupInput, iam.CreateGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	return echoIAMCall[iam.CreateInstanceProfileInput, iam.CreateInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateOpenIDConnectProvider(accountID string, input *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error) {
	return echoIAMCall[iam.CreateOpenIDConnectProviderInput, iam.CreateOpenIDConnectProviderOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return echoIAMCall[iam.CreateRoleInput, iam.CreateRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateUser(accountID string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	return echoIAMCall[iam.CreateUserInput, iam.CreateUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	return echoIAMCall[iam.DeleteAccessKeyInput, iam.DeleteAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteGroupPolicy(accountID string, input *iam.DeleteGroupPolicyInput) (*iam.DeleteGroupPolicyOutput, error) {
	return echoIAMCall[iam.DeleteGroupPolicyInput, iam.DeleteGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	return echoIAMCall[iam.DeleteInstanceProfileInput, iam.DeleteInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteRole(accountID string, input *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	return echoIAMCall[iam.DeleteRoleInput, iam.DeleteRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteRolePolicy(accountID string, input *iam.DeleteRolePolicyInput) (*iam.DeleteRolePolicyOutput, error) {
	return echoIAMCall[iam.DeleteRolePolicyInput, iam.DeleteRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteUser(accountID string, input *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	return echoIAMCall[iam.DeleteUserInput, iam.DeleteUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteUserPolicy(accountID string, input *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	return echoIAMCall[iam.DeleteUserPolicyInput, iam.DeleteUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	if _, ok := svc.wantOutput.(*iam.GetInstanceProfileOutput); !ok {
		return &iam.GetInstanceProfileOutput{InstanceProfile: &iam.InstanceProfile{}}, nil
	}
	return echoIAMCall[iam.GetInstanceProfileInput, iam.GetInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return echoIAMCall[iam.GetRoleInput, iam.GetRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return echoIAMCall[iam.GetUserInput, iam.GetUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListAccessKeys(accountID string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	return echoIAMCall[iam.ListAccessKeysInput, iam.ListAccessKeysOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroupPolicies(accountID string, input *iam.ListGroupPoliciesInput) (*iam.ListGroupPoliciesOutput, error) {
	return echoIAMCall[iam.ListGroupPoliciesInput, iam.ListGroupPoliciesOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroups(accountID string, input *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	return echoIAMCall[iam.ListGroupsInput, iam.ListGroupsOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroupsForUser(accountID string, input *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	return echoIAMCall[iam.ListGroupsForUserInput, iam.ListGroupsForUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListUsers(accountID string, input *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	return echoIAMCall[iam.ListUsersInput, iam.ListUsersOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutGroupPolicy(accountID string, input *iam.PutGroupPolicyInput) (*iam.PutGroupPolicyOutput, error) {
	return echoIAMCall[iam.PutGroupPolicyInput, iam.PutGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutRolePolicy(accountID string, input *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	return echoIAMCall[iam.PutRolePolicyInput, iam.PutRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutUserPolicy(accountID string, input *iam.PutUserPolicyInput) (*iam.PutUserPolicyOutput, error) {
	return echoIAMCall[iam.PutUserPolicyInput, iam.PutUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return echoIAMCall[iam.RemoveRoleFromInstanceProfileInput, iam.RemoveRoleFromInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) RemoveUserFromGroup(accountID string, input *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	return echoIAMCall[iam.RemoveUserFromGroupInput, iam.RemoveUserFromGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	return echoIAMCall[iam.UpdateAccessKeyInput, iam.UpdateAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	return echoIAMCall[iam.UpdateAssumeRolePolicyInput, iam.UpdateAssumeRolePolicyOutput](svc, accountID, input)
}

var _ handlers_iam.IAMService = (*echoIAMService)(nil)
