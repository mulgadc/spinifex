// Package awsmodel loads the AWS Smithy service models used by the
// conformance suite and translates them into the api-2.json shaped structs
// its validators read.
package awsmodel

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
)

// ModelRepository is where the embedded models come from. The commit is
// recorded in model_source.go by scripts/sync-aws-models.sh.
const ModelRepository = "github.com/aws/api-models-aws"

// ModelSourceDescription names the pinned model source for reports.
func ModelSourceDescription() string {
	return fmt.Sprintf("%s@%.12s, %s", ModelRepository, ModelCommit, ModelCommitDate)
}

// Service identifies an AWS service model embedded in this package.
type Service string

const (
	ACM                    Service = "acm"
	EC2                    Service = "ec2"
	ECR                    Service = "ecr"
	ECS                    Service = "ecs"
	EKS                    Service = "eks"
	ElasticLoadBalancingV2 Service = "elasticloadbalancingv2"
	IAM                    Service = "iam"
	RDS                    Service = "rds"
	S3                     Service = "s3"
	STS                    Service = "sts"
)

// Metadata describes the service and wire protocol represented by a model.
// Protocol uses the api-2.json names: json, rest-json, rest-xml, query, ec2.
type Metadata struct {
	APIVersion      string
	EndpointPrefix  string
	Protocol        string
	ServiceFullName string
	ServiceID       string
}

// Operation describes an AWS API operation and the shapes used by its input,
// output and declared errors.
type Operation struct {
	Name   string
	HTTP   HTTP
	Input  *ShapeRef
	Output *ShapeRef
	Errors []ShapeRef
}

// HTTP describes an operation's HTTP binding.
type HTTP struct {
	Method       string
	RequestURI   string
	ResponseCode int
}

// Shape describes a value in an AWS service model. Depending on Type, it can
// refer to structure members, a list member, or map keys and values.
type Shape struct {
	Type            string
	Required        []string
	Members         map[string]ShapeRef
	Member          *ShapeRef
	Key             *ShapeRef
	Value           *ShapeRef
	Enum            []string
	Min             *float64
	Max             *float64
	Pattern         string
	LocationName    string
	TimestampFormat string
	Payload         string
	Flattened       bool
	Sensitive       bool
	Exception       bool
	Fault           bool
	Error           *ErrorInfo
}

// ErrorInfo describes an error shape's code and HTTP classification.
type ErrorInfo struct {
	Code           string
	HTTPStatusCode int
	SenderFault    bool
}

// ShapeRef names another shape and carries any wire binding specific to the
// place where it is referenced.
type ShapeRef struct {
	Shape           string
	ResultWrapper   string
	Location        string
	LocationName    string
	QueryName       string
	TimestampFormat string
	Flattened       bool
	Streaming       bool
	XMLAttribute    bool
}

// Model is an indexed AWS service definition.
type Model struct {
	service    Service
	metadata   Metadata
	operations map[string]*Operation
	shapes     map[string]*Shape
}

type loadResult struct {
	model *Model
	err   error
}

var modelCache sync.Map

// Services returns the supported service identifiers in stable order.
func Services() []Service {
	services := make([]Service, 0, len(modelFiles))
	for service := range modelFiles {
		services = append(services, service)
	}
	slices.Sort(services)
	return services
}

// Load parses and indexes the embedded model for service. Each service is
// parsed at most once and the resulting Model is safe for concurrent reads.
func Load(service Service) (*Model, error) {
	file, ok := modelFiles[service]
	if !ok {
		return nil, fmt.Errorf("awsmodel: unsupported service %q", service)
	}

	loader, _ := modelCache.LoadOrStore(service, sync.OnceValue(func() loadResult {
		contents, err := readModel(file)
		if err != nil {
			return loadResult{err: fmt.Errorf("awsmodel: read %s model: %w", service, err)}
		}
		model, err := parseSmithyModel(service, contents)
		return loadResult{model: model, err: err}
	}))
	load, ok := loader.(func() loadResult)
	if !ok {
		return nil, fmt.Errorf("awsmodel: invalid cache entry for service %q", service)
	}
	result := load()
	return result.model, result.err
}

func readModel(file string) ([]byte, error) {
	compressed, err := modelFS.ReadFile(file)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

// Service returns the identifier used to load the model.
func (m *Model) Service() Service { return m.service }

// Metadata returns the service metadata.
func (m *Model) Metadata() Metadata { return m.metadata }

// Operation resolves an operation by its model key, such as DescribeInstances.
func (m *Model) Operation(name string) (*Operation, bool) {
	operation, ok := m.operations[name]
	return operation, ok
}

// Shape resolves a shape by name.
func (m *Model) Shape(name string) (*Shape, bool) {
	shape, ok := m.shapes[name]
	return shape, ok
}

// Operations returns all operation keys in stable order.
func (m *Model) Operations() []string { return slices.Sorted(maps.Keys(m.operations)) }

// Shapes returns all shape names in stable order.
func (m *Model) Shapes() []string { return slices.Sorted(maps.Keys(m.shapes)) }

func (m *Model) validateReferences() error {
	validate := func(owner string, ref *ShapeRef) error {
		if ref == nil {
			return nil
		}
		if _, ok := m.shapes[ref.Shape]; !ok {
			return fmt.Errorf("awsmodel: %s model %s references unknown shape %q", m.service, owner, ref.Shape)
		}
		return nil
	}

	for name, operation := range m.operations {
		if operation == nil {
			return fmt.Errorf("awsmodel: %s model operation %q is null", m.service, name)
		}
		if err := validate("operation "+name+" input", operation.Input); err != nil {
			return err
		}
		if err := validate("operation "+name+" output", operation.Output); err != nil {
			return err
		}
		for i := range operation.Errors {
			if err := validate(fmt.Sprintf("operation %s error %d", name, i), &operation.Errors[i]); err != nil {
				return err
			}
		}
	}

	for name, shape := range m.shapes {
		if shape == nil {
			return fmt.Errorf("awsmodel: %s model shape %q is null", m.service, name)
		}
		for memberName, member := range shape.Members {
			if err := validate("shape "+name+" member "+memberName, &member); err != nil {
				return err
			}
		}
		for label, ref := range map[string]*ShapeRef{
			"member": shape.Member,
			"key":    shape.Key,
			"value":  shape.Value,
		} {
			if err := validate("shape "+name+" "+label, ref); err != nil {
				return err
			}
		}
	}
	return nil
}
