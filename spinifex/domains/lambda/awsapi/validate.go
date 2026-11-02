package awsapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const (
	supportedRuntime  = "provided.al2023"
	defaultTimeout    = 3
	maxTimeout        = 900
	defaultMemory     = 128
	maxMemory         = 10240
	maxDescription    = 256
	maxEnvironmentLen = 4096
	// maxBodyBytes bounds a CreateFunction body: the base64 of the largest
	// code copy plus room for the rest of the document.
	maxBodyBytes = (codestore.MaxCodeBytes/3+1)*4 + 1<<20
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9-_]{1,64}$`)

func invalid(format string, args ...any) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, format, args...)
}

func unsupported(field string) error {
	return invalid("%s is not supported", field)
}

// decodeCreate reads a CreateFunction body strictly: a member this build does
// not model is refused rather than silently dropped.
func decodeCreate(body []byte) (*lambda.CreateFunctionInput, error) {
	in := new(lambda.CreateFunctionInput)
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(in); err != nil {
		return nil, invalid("the request body is not a valid CreateFunction document: %v", err)
	}
	return in, nil
}

// createParams is a CreateFunction request after validation, defaults applied.
type createParams struct {
	Name        string
	Role        string
	Handler     string
	Description string
	TimeoutSec  int64
	MemorySize  int64
	Code        []byte
	Env         map[string]string
}

// isSet reports whether a modelled optional member carries anything: a non-nil
// pointer to a non-zero value, a non-empty list or map, or a struct with any
// such member.
func isSet(v any) bool {
	return valueSet(reflect.ValueOf(v))
}

func valueSet(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && valueSet(v.Elem())
	case reflect.Slice, reflect.Map:
		return v.Len() > 0
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() && valueSet(v.Field(i)) {
				return true
			}
		}
		return false
	case reflect.Invalid:
		return false
	default:
		return !v.IsZero()
	}
}

// refuseUnsupported names the first supplied member this build cannot honour.
func refuseUnsupported(in *lambda.CreateFunctionInput) error {
	checks := []struct {
		field string
		set   bool
	}{
		{"Code.S3Bucket", in.Code != nil && isSet(in.Code.S3Bucket)},
		{"Code.S3Key", in.Code != nil && isSet(in.Code.S3Key)},
		{"Code.S3ObjectVersion", in.Code != nil && isSet(in.Code.S3ObjectVersion)},
		{"Code.ImageUri", in.Code != nil && isSet(in.Code.ImageUri)},
		{"Layers", isSet(in.Layers)},
		{"VpcConfig", isSet(in.VpcConfig)},
		{"KMSKeyArn", isSet(in.KMSKeyArn)},
		{"DeadLetterConfig", isSet(in.DeadLetterConfig)},
		{"TracingConfig", in.TracingConfig != nil && aws.StringValue(in.TracingConfig.Mode) == lambda.TracingModeActive},
		{"FileSystemConfigs", isSet(in.FileSystemConfigs)},
		{"ImageConfig", isSet(in.ImageConfig)},
		{"Architectures", isSet(in.Architectures) && (len(in.Architectures) != 1 || aws.StringValue(in.Architectures[0]) != lambda.ArchitectureX8664)},
		{"Publish", aws.BoolValue(in.Publish)},
		{"SnapStart", in.SnapStart != nil && aws.StringValue(in.SnapStart.ApplyOn) != "" && aws.StringValue(in.SnapStart.ApplyOn) != lambda.SnapStartApplyOnNone},
		{"EphemeralStorage", in.EphemeralStorage != nil && in.EphemeralStorage.Size != nil && aws.Int64Value(in.EphemeralStorage.Size) != 512},
		{"CodeSigningConfigArn", isSet(in.CodeSigningConfigArn)},
		{"Tags", isSet(in.Tags)},
	}
	for _, c := range checks {
		if c.set {
			return unsupported(c.field)
		}
	}
	return nil
}

// validateCreate checks a decoded CreateFunction and applies the defaults.
// resolveFunctionName accepts the GetFunction forms AWS does: a name, a partial
// ARN (account:function:name) or a full ARN, each with an optional qualifier.
// Another account or Region is reported not found under the ARN it named.
func resolveFunctionName(region, accountID, raw string) (name, qualifier string, err error) {
	parts := strings.Split(raw, ":")
	qualified := false
	switch {
	case len(parts) <= 2:
		name = parts[0]
		if len(parts) == 2 {
			qualifier, qualified = parts[1], true
		}
	case len(parts) <= 4 && parts[1] == "function":
		if parts[0] != accountID {
			return "", "", awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "Function not found: %s",
				arn.FormatLambdaFunction(region, parts[0], strings.Join(parts[2:], ":")))
		}
		name = parts[2]
		if len(parts) == 4 {
			qualifier, qualified = parts[3], true
		}
	case len(parts) >= 7 && len(parts) <= 8 && parts[0] == "arn" && parts[2] == "lambda" && parts[5] == "function":
		if parts[1] != "aws" || parts[3] != region || parts[4] != accountID {
			return "", "", awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "Function not found: %s", raw)
		}
		name = parts[6]
		if len(parts) == 8 {
			qualifier, qualified = parts[7], true
		}
	default:
		return "", "", invalidFunctionName()
	}
	if !nameRE.MatchString(name) || (qualified && qualifier == "") {
		return "", "", invalidFunctionName()
	}
	return name, qualifier, nil
}

func invalidFunctionName() error {
	return invalid("1 validation error detected: Value at 'functionName' failed to satisfy constraint: Member must satisfy regular expression pattern: (arn:(aws[a-zA-Z-]*)?:lambda:)?([a-z]{2}(-gov)?-[a-z]+-\\d{1}:)?(\\d{12}:)?(function:)?([a-zA-Z0-9-_]+)(:(\\$LATEST|[a-zA-Z0-9-_]+))?")
}

// logGroup is the log group AWS names for a function by default.
func logGroup(name string) string { return "/aws/lambda/" + name }

// defaultLogging reports whether c is absent or spells AWS's default exactly;
// without a logs capability any other value would be inert.
func defaultLogging(c *lambda.LoggingConfig, name string) bool {
	if c == nil {
		return true
	}
	format, group := aws.StringValue(c.LogFormat), aws.StringValue(c.LogGroup)
	return (format == "" || format == lambda.LogFormatText) && (group == "" || group == logGroup(name)) &&
		c.ApplicationLogLevel == nil && c.SystemLogLevel == nil
}

func validateCreate(in *lambda.CreateFunctionInput) (*createParams, error) {
	if err := refuseUnsupported(in); err != nil {
		return nil, err
	}
	name := aws.StringValue(in.FunctionName)
	if strings.HasPrefix(name, "arn:") || !nameRE.MatchString(name) {
		return nil, invalid("1 validation error detected: Value at 'functionName' failed to satisfy constraint: Member must satisfy regular expression pattern: [a-zA-Z0-9-_]+ and have length 1 to 64")
	}
	if !defaultLogging(in.LoggingConfig, name) {
		return nil, unsupported("A LoggingConfig other than LogFormat Text with log group " + logGroup(name))
	}
	if aws.StringValue(in.Runtime) != supportedRuntime {
		return nil, invalid("Runtime must be %s", supportedRuntime)
	}
	if pt := aws.StringValue(in.PackageType); pt != "" && pt != lambda.PackageTypeZip {
		return nil, invalid("PackageType must be %s", lambda.PackageTypeZip)
	}
	if in.Code == nil || len(in.Code.ZipFile) == 0 {
		return nil, invalid("Code.ZipFile is required")
	}
	if len(in.Code.ZipFile) > codestore.MaxCodeBytes {
		return nil, awserrors.Errorf(awserrors.ErrorRequestEntityTooLarge, "Code.ZipFile exceeds %d bytes", codestore.MaxCodeBytes)
	}
	if _, err := zip.NewReader(bytes.NewReader(in.Code.ZipFile), int64(len(in.Code.ZipFile))); err != nil {
		return nil, invalid("Could not unzip uploaded file. Please check your file, then try to upload again.")
	}
	handler := aws.StringValue(in.Handler)
	if handler == "" {
		return nil, invalid("Handler is required")
	}
	role := aws.StringValue(in.Role)
	if role == "" {
		return nil, invalid("Role is required")
	}
	limit, memory := int64(defaultTimeout), int64(defaultMemory)
	if in.Timeout != nil {
		limit = *in.Timeout
	}
	if limit < 1 || limit > maxTimeout {
		return nil, invalid("Timeout must be between 1 and %d", maxTimeout)
	}
	if in.MemorySize != nil {
		memory = *in.MemorySize
	}
	if memory < 128 || memory > maxMemory {
		return nil, invalid("MemorySize must be between 128 and %d", maxMemory)
	}
	desc := aws.StringValue(in.Description)
	if len(desc) > maxDescription {
		return nil, invalid("Description must be at most %d characters", maxDescription)
	}
	env, err := envVars(in.Environment)
	if err != nil {
		return nil, err
	}
	return &createParams{
		Name: name, Role: role, Handler: handler, Description: desc,
		TimeoutSec: limit, MemorySize: memory, Code: in.Code.ZipFile, Env: env,
	}, nil
}

func envVars(e *lambda.Environment) (map[string]string, error) {
	if e == nil || len(e.Variables) == 0 {
		return nil, nil
	}
	vars := make(map[string]string, len(e.Variables))
	size := 0
	for k, v := range e.Variables {
		if k == "" {
			return nil, invalid("Environment variable names must not be empty")
		}
		vars[k] = aws.StringValue(v)
		size += len(k) + len(vars[k])
	}
	if size > maxEnvironmentLen {
		return nil, invalid("Environment variables exceed %d bytes", maxEnvironmentLen)
	}
	return vars, nil
}
