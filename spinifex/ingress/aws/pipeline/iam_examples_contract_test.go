package pipeline

import (
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
)

func TestIAMExamplesContract(t *testing.T) {
	examples := loadIAMExamples(t)
	covered := 0

	for action, actionExamples := range examples.Examples {
		if !slices.Contains(iamawsapi.ActionNames(), action) {
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

	require.NotZero(t, covered, "no IAM examples were covered — check testdata path")
}
