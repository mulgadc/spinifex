// Package sdkv1 reports which operations the vendored aws-sdk-go v1 knows.
//
// Spinifex is built on that frozen SDK, so an operation absent from it cannot
// be implemented or exercised with the SDK the repository tests against. The
// set is read from the SDK's own client types, never listed by hand.
package sdkv1

import (
	"reflect"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/sts"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

var clients = map[awsmodel.Service]reflect.Type{
	awsmodel.ACM:                    reflect.TypeFor[*acm.ACM](),
	awsmodel.EC2:                    reflect.TypeFor[*ec2.EC2](),
	awsmodel.ECR:                    reflect.TypeFor[*ecr.ECR](),
	awsmodel.ECS:                    reflect.TypeFor[*ecs.ECS](),
	awsmodel.EKS:                    reflect.TypeFor[*eks.EKS](),
	awsmodel.ElasticLoadBalancingV2: reflect.TypeFor[*elbv2.ELBV2](),
	awsmodel.IAM:                    reflect.TypeFor[*iam.IAM](),
	awsmodel.RDS:                    reflect.TypeFor[*rds.RDS](),
	awsmodel.Route53:                reflect.TypeFor[*route53.Route53](),
	awsmodel.S3:                     reflect.TypeFor[*s3.S3](),
	awsmodel.SQS:                    reflect.TypeFor[*sqs.SQS](),
	awsmodel.STS:                    reflect.TypeFor[*sts.STS](),
}

var requestType = reflect.TypeFor[*request.Request]()

// Operations returns the sorted operation names the SDK v1 client for service
// exposes, and false when the service has no SDK v1 client registered here.
// Every operation has an XxxRequest method that builds its request.
func Operations(service awsmodel.Service) ([]string, bool) {
	client, ok := clients[service]
	if !ok {
		return nil, false
	}
	var operations []string
	for method := range client.Methods() {
		name, isRequest := strings.CutSuffix(method.Name, "Request")
		if !isRequest || name == "" {
			continue
		}
		// Receiver plus input, returning the request and the output.
		if method.Type.NumIn() != 2 || method.Type.NumOut() != 2 || method.Type.Out(0) != requestType {
			continue
		}
		operations = append(operations, name)
	}
	slices.Sort(operations)
	return operations, true
}
