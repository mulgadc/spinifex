package awsmodel

import "embed"

// modelFS holds the Smithy service models, gzipped, from the api-models-aws
// commit named by ModelCommit, and the curated EC2 error catalog.
//
//go:embed models/*.json.gz models/ec2/error-codes.json
var modelFS embed.FS

var modelFiles = map[Service]string{
	ACM:                    "models/acm-2015-12-08.json.gz",
	EC2:                    "models/ec2-2016-11-15.json.gz",
	ECR:                    "models/ecr-2015-09-21.json.gz",
	ECS:                    "models/ecs-2014-11-13.json.gz",
	EKS:                    "models/eks-2017-11-01.json.gz",
	ElasticLoadBalancingV2: "models/elastic-load-balancing-v2-2015-12-01.json.gz",
	IAM:                    "models/iam-2010-05-08.json.gz",
	RDS:                    "models/rds-2014-10-31.json.gz",
	S3:                     "models/s3-2006-03-01.json.gz",
	STS:                    "models/sts-2011-06-15.json.gz",
}
