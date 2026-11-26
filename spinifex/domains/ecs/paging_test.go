package ecs

import (
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/domains/ecs/taskdefinition"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listFunc calls one paged ECS action and returns its items as ARNs.
type listFunc func(t *testing.T, svc *Service, maxResults *int64, token *string) ([]string, *string, error)

// pagedAction seeds n listable items, each a filter excludes sorting first, and
// returns the ARNs the action must list.
type pagedAction struct {
	name     string
	def, max int64
	seed     func(t *testing.T, svc *Service, n int) []string
	list     listFunc
}

func putJSON(t *testing.T, svc *Service, key string, v any) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, taskdefinition.PutJSON(t.Context(), kv, key, v))
}

func createCluster(t *testing.T, svc *Service, name string) string {
	t.Helper()
	out, err := svc.CreateCluster(t.Context(), &ecs.CreateClusterInput{ClusterName: aws.String(name)}, testAccountID)
	require.NoError(t, err)
	return aws.StringValue(out.Cluster.ClusterArn)
}

var pagedActions = []pagedAction{
	{
		name: "ListClusters", def: 100, max: 100,
		seed: func(t *testing.T, svc *Service, n int) []string {
			want := make([]string, 0, n)
			for i := range n {
				want = append(want, createCluster(t, svc, fmt.Sprintf("c-%03d", i)))
			}
			return want
		},
		list: func(t *testing.T, svc *Service, maxResults *int64, token *string) ([]string, *string, error) {
			out, err := svc.ListClusters(t.Context(), &ecs.ListClustersInput{MaxResults: maxResults, NextToken: token}, testAccountID)
			if err != nil {
				return nil, nil, err
			}
			return aws.StringValueSlice(out.ClusterArns), out.NextToken, nil
		},
	},
	{
		name: "ListServices", def: 10, max: 100,
		seed: func(t *testing.T, svc *Service, n int) []string {
			createCluster(t, svc, "web")
			createCluster(t, svc, "other")
			putJSON(t, svc, ServiceKey("other", "a"), &ServiceRecord{Name: "a", Cluster: "other",
				ARN: ServiceARN(testRegion, testAccountID, "other", "a"), Status: ServiceStatusActive})
			want := make([]string, 0, n)
			for i := range n {
				name := fmt.Sprintf("s-%03d", i)
				arn := ServiceARN(testRegion, testAccountID, "web", name)
				putJSON(t, svc, ServiceKey("web", name), &ServiceRecord{Name: name, Cluster: "web", ARN: arn, Status: ServiceStatusActive})
				want = append(want, arn)
			}
			return want
		},
		list: func(t *testing.T, svc *Service, maxResults *int64, token *string) ([]string, *string, error) {
			out, err := svc.ListServices(t.Context(), &ecs.ListServicesInput{Cluster: aws.String("web"), MaxResults: maxResults, NextToken: token}, testAccountID)
			if err != nil {
				return nil, nil, err
			}
			return aws.StringValueSlice(out.ServiceArns), out.NextToken, nil
		},
	},
	{
		name: "ListTasks", def: 100, max: 100,
		seed: func(t *testing.T, svc *Service, n int) []string {
			createCluster(t, svc, "web")
			put := func(id, desired string) string {
				arn := TaskARN(testRegion, testAccountID, "web", id)
				putJSON(t, svc, TaskKey("web", id), &TaskRecord{TaskID: id, Cluster: "web", ARN: arn, DesiredStatus: desired})
				return arn
			}
			put("a-stopped", TaskStatusStopped)
			want := make([]string, 0, n)
			for i := range n {
				want = append(want, put(fmt.Sprintf("t-%03d", i), TaskStatusRunning))
			}
			return want
		},
		list: func(t *testing.T, svc *Service, maxResults *int64, token *string) ([]string, *string, error) {
			out, err := svc.ListTasks(t.Context(), &ecs.ListTasksInput{Cluster: aws.String("web"), MaxResults: maxResults, NextToken: token}, testAccountID)
			if err != nil {
				return nil, nil, err
			}
			return aws.StringValueSlice(out.TaskArns), out.NextToken, nil
		},
	},
	{
		name: "ListTaskDefinitions", def: 100, max: 100,
		seed: func(t *testing.T, svc *Service, n int) []string {
			registerTaskDef(t, svc, "a-gone", 128, 256)
			_, err := svc.DeregisterTaskDefinition(t.Context(), &ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String("a-gone:1")}, testAccountID)
			require.NoError(t, err)
			want := make([]string, 0, n)
			for i := range n {
				out := registerTaskDef(t, svc, fmt.Sprintf("f-%03d", i), 128, 256)
				want = append(want, aws.StringValue(out.TaskDefinition.TaskDefinitionArn))
			}
			return want
		},
		list: func(t *testing.T, svc *Service, maxResults *int64, token *string) ([]string, *string, error) {
			out, err := svc.ListTaskDefinitions(t.Context(), &ecs.ListTaskDefinitionsInput{MaxResults: maxResults, NextToken: token}, testAccountID)
			if err != nil {
				return nil, nil, err
			}
			return aws.StringValueSlice(out.TaskDefinitionArns), out.NextToken, nil
		},
	},
	{
		name: "ListContainerInstances", def: 100, max: 100,
		seed: func(t *testing.T, svc *Service, n int) []string {
			createCluster(t, svc, "web")
			registerInstance(t, svc, "web", "i-a-drained", 1024, 2048)
			kv, err := svc.bucket(t.Context(), testAccountID)
			require.NoError(t, err)
			_, err = svc.upsertInstance(t.Context(), kv, testAccountID, "web", "i-a-drained", func(r *InstanceRecord) {
				r.Status = InstanceStatusDraining
			})
			require.NoError(t, err)
			want := make([]string, 0, n)
			for i := range n {
				id := fmt.Sprintf("i-%03d", i)
				registerInstance(t, svc, "web", id, 1024, 2048)
				want = append(want, ContainerInstanceARN(testRegion, testAccountID, "web", id))
			}
			return want
		},
		list: func(t *testing.T, svc *Service, maxResults *int64, token *string) ([]string, *string, error) {
			out, err := svc.ListContainerInstances(t.Context(), &ecs.ListContainerInstancesInput{
				Cluster: aws.String("web"), Status: aws.String(InstanceStatusActive), MaxResults: maxResults, NextToken: token,
			}, testAccountID)
			if err != nil {
				return nil, nil, err
			}
			return aws.StringValueSlice(out.ContainerInstanceArns), out.NextToken, nil
		},
	},
	{
		name: "DescribeCapacityProviders", def: 10, max: 10,
		seed: func(t *testing.T, svc *Service, n int) []string {
			want := make([]string, 0, n)
			for i := range n {
				out, err := svc.CreateCapacityProvider(t.Context(), &ecs.CreateCapacityProviderInput{
					Name: aws.String(fmt.Sprintf("cp-%03d", i)),
					AutoScalingGroupProvider: &ecs.AutoScalingGroupProvider{
						AutoScalingGroupArn: aws.String("arn:aws:autoscaling:ap-southeast-2:123456789012:autoScalingGroup:x:autoScalingGroupName/asg"),
					},
				}, testAccountID)
				require.NoError(t, err)
				want = append(want, aws.StringValue(out.CapacityProvider.CapacityProviderArn))
			}
			return want
		},
		list: func(t *testing.T, svc *Service, maxResults *int64, token *string) ([]string, *string, error) {
			out, err := svc.DescribeCapacityProviders(t.Context(), &ecs.DescribeCapacityProvidersInput{MaxResults: maxResults, NextToken: token}, testAccountID)
			if err != nil {
				return nil, nil, err
			}
			arns := make([]string, 0, len(out.CapacityProviders))
			for _, cp := range out.CapacityProviders {
				arns = append(arns, aws.StringValue(cp.CapacityProviderArn))
			}
			return arns, out.NextToken, nil
		},
	},
}

// Every page but the last is full and the filtered-out item sorts first, so
// a page cut before filtering would come up short.
func TestPaging_WalkSeesEachItemOnce(t *testing.T) {
	t.Parallel()
	for _, a := range pagedActions {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := newTestService(t)
			want := a.seed(t, svc, 7)
			size := aws.Int64(3)
			var seen []string
			var token *string
			pages := 0
			for range len(want) + 1 {
				page, next, err := a.list(t, svc, size, token)
				require.NoError(t, err)
				pages++
				seen = append(seen, page...)
				if next == nil {
					break
				}
				require.Len(t, page, 3)
				token = next
			}
			assert.Equal(t, 3, pages)
			assert.ElementsMatch(t, want, seen)
		})
	}
}

func TestPaging_OmittedMaxResultsUsesAWSDefault(t *testing.T) {
	t.Parallel()
	for _, a := range pagedActions {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := newTestService(t)
			a.seed(t, svc, int(a.def)+1)
			page, next, err := a.list(t, svc, nil, nil)
			require.NoError(t, err)
			assert.Len(t, page, int(a.def))
			assert.NotNil(t, next)
		})
	}
}

func TestPaging_RefusesBadMaxResultsAndForeignToken(t *testing.T) {
	t.Parallel()
	for _, a := range pagedActions {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := newTestService(t)
			a.seed(t, svc, 1)
			_, _, err := a.list(t, svc, aws.Int64(a.max), nil)
			require.NoError(t, err)
			for _, req := range []struct {
				max   *int64
				token *string
			}{
				{max: aws.Int64(0)},
				{max: aws.Int64(a.max + 1)},
				{token: aws.String("not-our-token")},
			} {
				_, _, err := a.list(t, svc, req.max, req.token)
				code, _ := awserrors.ResolveErrorCode(err)
				assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
			}
		})
	}
}
