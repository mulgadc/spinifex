package awsmodel

import "embed"

// modelFS contains only the curated EC2 error catalog. The large upstream
// api-2.json files are resolved from the pinned aws-sdk-go module cache at
// runtime; they are intentionally not source-controlled here.
//
//go:embed models/ec2/error-codes.json
var modelFS embed.FS

var modelFiles = map[Service]string{
	ACM:                    "models/apis/acm/2015-12-08/api-2.json",
	EC2:                    "models/apis/ec2/2016-11-15/api-2.json",
	ECR:                    "models/apis/ecr/2015-09-21/api-2.json",
	ECS:                    "models/apis/ecs/2014-11-13/api-2.json",
	ElasticLoadBalancingV2: "models/apis/elasticloadbalancingv2/2015-12-01/api-2.json",
	IAM:                    "models/apis/iam/2010-05-08/api-2.json",
	S3:                     "models/apis/s3/2006-03-01/api-2.json",
	STS:                    "models/apis/sts/2011-06-15/api-2.json",
}
