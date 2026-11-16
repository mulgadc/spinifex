package ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RunTask and StartTask refuse, with AWS's code and text, a cluster that has no
// registered container instance and an awsvpc task with no networkConfiguration.
func TestLaunch_RefusalsMatchAWS(t *testing.T) {
	const (
		noInstances = "No Container Instances were found in your cluster."
		noNetConfig = "Network Configuration must be provided when networkMode 'awsvpc' is specified."
	)
	launches := map[string]func(*Service, string) error{
		"RunTask": func(svc *Service, family string) error {
			_, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
				Cluster: aws.String("web"), TaskDefinition: aws.String(family),
			}, testAccountID)
			return err
		},
		"StartTask": func(svc *Service, family string) error {
			_, err := svc.StartTask(context.Background(), &ecs.StartTaskInput{
				Cluster: aws.String("web"), TaskDefinition: aws.String(family),
				ContainerInstances: aws.StringSlice([]string{"i-1"}),
			}, testAccountID)
			return err
		},
	}
	cases := []struct {
		name, family, msg string
		withInstance      bool
	}{
		{name: "noCapacity", family: "bridge", msg: noInstances},
		{name: "noNetworkConfiguration", family: "vpc", msg: noNetConfig, withInstance: true},
	}
	for action, launch := range launches {
		for _, tc := range cases {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				svc, _ := newTestService(t)
				eni := &stubENI{}
				svc.eni = eni
				_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
				require.NoError(t, err)
				registerTaskDef(t, svc, "bridge", 128, 256)
				registerAwsvpcTaskDef(t, svc, "vpc", 128, 256)
				if tc.withInstance {
					registerInstance(t, svc, "web", "i-1", 1024, 2048)
				}

				code, msg, ok := awserrors.ResolveErrorDetail(launch(svc, tc.family))
				require.True(t, ok)
				assert.Equal(t, "InvalidParameterException", code)
				assert.Equal(t, tc.msg, msg)
				assert.Equal(t, 0, eni.allocCalls)
			})
		}
	}
}
