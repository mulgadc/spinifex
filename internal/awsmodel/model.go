// Package awsmodel loads the AWS api-2.json service definitions used by the
// conformance suite.
package awsmodel

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"sync"
)

// SourceSDKVersion is the aws-sdk-go release whose module-cache models are
// used by the conformance suite.
const SourceSDKVersion = "v1.55.8"

const sourceSDKModule = "github.com/aws/aws-sdk-go"

// Service identifies an AWS service model loaded from the SDK module cache.
type Service string

const (
	ACM                    Service = "acm"
	EC2                    Service = "ec2"
	ECR                    Service = "ecr"
	ECS                    Service = "ecs"
	ElasticLoadBalancingV2 Service = "elasticloadbalancingv2"
	IAM                    Service = "iam"
	RDS                    Service = "rds"
	S3                     Service = "s3"
	STS                    Service = "sts"
)

// Metadata describes the service and wire protocol represented by a model.
type Metadata struct {
	APIVersion          string   `json:"apiVersion"`
	EndpointPrefix      string   `json:"endpointPrefix"`
	Protocol            string   `json:"protocol"`
	Protocols           []string `json:"protocols"`
	ServiceAbbreviation string   `json:"serviceAbbreviation"`
	ServiceFullName     string   `json:"serviceFullName"`
	ServiceID           string   `json:"serviceId"`
	SignatureVersion    string   `json:"signatureVersion"`
	UID                 string   `json:"uid"`
}

// Operation describes an AWS API operation and the shapes used by its input,
// output and declared errors.
type Operation struct {
	Name   string     `json:"name"`
	HTTP   HTTP       `json:"http"`
	Input  *ShapeRef  `json:"input"`
	Output *ShapeRef  `json:"output"`
	Errors []ShapeRef `json:"errors"`
}

// HTTP describes an operation's HTTP binding.
type HTTP struct {
	Method       string `json:"method"`
	RequestURI   string `json:"requestUri"`
	ResponseCode int    `json:"responseCode"`
}

// Shape describes a value in an AWS service model. Depending on Type, it can
// refer to structure members, a list member, or map keys and values.
type Shape struct {
	Type            string              `json:"type"`
	Required        []string            `json:"required"`
	Members         map[string]ShapeRef `json:"members"`
	Member          *ShapeRef           `json:"member"`
	Key             *ShapeRef           `json:"key"`
	Value           *ShapeRef           `json:"value"`
	Enum            []string            `json:"enum"`
	Min             *float64            `json:"min"`
	Max             *float64            `json:"max"`
	Pattern         string              `json:"pattern"`
	LocationName    string              `json:"locationName"`
	TimestampFormat string              `json:"timestampFormat"`
	Payload         string              `json:"payload"`
	Flattened       bool                `json:"flattened"`
	Sensitive       bool                `json:"sensitive"`
	Exception       bool                `json:"exception"`
	Fault           bool                `json:"fault"`
	Error           *ErrorInfo          `json:"error"`
}

// ErrorInfo describes an error shape's code and HTTP classification.
type ErrorInfo struct {
	Code           string `json:"code"`
	HTTPStatusCode int    `json:"httpStatusCode"`
	SenderFault    bool   `json:"senderFault"`
}

// ShapeRef names another shape and carries any wire binding specific to the
// place where it is referenced.
type ShapeRef struct {
	Shape           string `json:"shape"`
	ResultWrapper   string `json:"resultWrapper"`
	Location        string `json:"location"`
	LocationName    string `json:"locationName"`
	QueryName       string `json:"queryName"`
	TimestampFormat string `json:"timestampFormat"`
	Flattened       bool   `json:"flattened"`
	Streaming       bool   `json:"streaming"`
	XMLAttribute    bool   `json:"xmlAttribute"`
}

// Model is an indexed AWS service definition.
type Model struct {
	service    Service
	metadata   Metadata
	operations map[string]*Operation
	shapes     map[string]*Shape
}

type modelDocument struct {
	Version    string                `json:"version"`
	Metadata   Metadata              `json:"metadata"`
	Operations map[string]*Operation `json:"operations"`
	Shapes     map[string]*Shape     `json:"shapes"`
}

type loadResult struct {
	model *Model
	err   error
}

var modelCache sync.Map

var modelRoot = sync.OnceValues(resolveModelRoot)

func resolveModelRoot() (string, error) {
	if root := os.Getenv("GOMODCACHE"); root != "" {
		return filepath.Join(root, sourceSDKModule+"@"+SourceSDKVersion), nil
	}
	if output, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
		if root := string(output); root != "" {
			return filepath.Join(stringTrimSpace(root), sourceSDKModule+"@"+SourceSDKVersion), nil
		}
	}
	currentUser, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("awsmodel: resolve Go module cache: %w", err)
	}
	return filepath.Join(currentUser.HomeDir, "go", "pkg", "mod", sourceSDKModule+"@"+SourceSDKVersion), nil
}

func stringTrimSpace(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r' || value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	return value
}

// Services returns the supported service identifiers in stable order.
func Services() []Service {
	services := make([]Service, 0, len(modelFiles))
	for service := range modelFiles {
		services = append(services, service)
	}
	slices.Sort(services)
	return services
}

// Load parses and indexes the cached SDK model for service. Each service is
// parsed at most once and the resulting Model is safe for concurrent reads.
func Load(service Service) (*Model, error) {
	path, ok := modelFiles[service]
	if !ok {
		return nil, fmt.Errorf("awsmodel: unsupported service %q", service)
	}

	loader, _ := modelCache.LoadOrStore(service, sync.OnceValue(func() loadResult {
		root, err := modelRoot()
		if err != nil {
			return loadResult{err: err}
		}
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return loadResult{err: fmt.Errorf("awsmodel: read cached %s model at %s: %w", service, filepath.Join(root, path), err)}
		}

		var document modelDocument
		if err := json.Unmarshal(contents, &document); err != nil {
			return loadResult{err: fmt.Errorf("awsmodel: parse %s model: %w", service, err)}
		}
		if document.Version != "2.0" {
			return loadResult{err: fmt.Errorf("awsmodel: %s model version is %q, want %q", service, document.Version, "2.0")}
		}
		if len(document.Operations) == 0 || len(document.Shapes) == 0 {
			return loadResult{err: fmt.Errorf("awsmodel: %s model has no operations or shapes", service)}
		}

		model := &Model{
			service:    service,
			metadata:   document.Metadata,
			operations: document.Operations,
			shapes:     document.Shapes,
		}
		if err := model.validateReferences(); err != nil {
			return loadResult{err: err}
		}
		return loadResult{model: model}
	}))
	load, ok := loader.(func() loadResult)
	if !ok {
		return nil, fmt.Errorf("awsmodel: invalid cache entry for service %q", service)
	}
	result := load()
	return result.model, result.err
}

// Service returns the identifier used to load the model.
func (m *Model) Service() Service { return m.service }

// Metadata returns the service metadata from api-2.json.
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
