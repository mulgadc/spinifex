package awsmodel_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	. "github.com/mulgadc/spinifex/internal/awsmodel"
)

func mustLoad(t *testing.T, service Service) *Model {
	t.Helper()
	model, err := Load(service)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func mustShape(t *testing.T, model *Model, name string) *Shape {
	t.Helper()
	shape, ok := model.Shape(name)
	if !ok {
		t.Fatalf("%s shape %q is not indexed", model.Service(), name)
	}
	return shape
}

func TestSmithyProtocols(t *testing.T) {
	want := map[Service]string{
		ACM: "json", EC2: "ec2", ECR: "json", ECS: "json", EKS: "rest-json",
		ElasticLoadBalancingV2: "query", IAM: "query", RDS: "query", S3: "rest-xml", STS: "query",
	}
	for service, protocol := range want {
		if got := mustLoad(t, service).Metadata().Protocol; got != protocol {
			t.Errorf("%s protocol = %q, want %q", service, got, protocol)
		}
	}
}

func TestSmithyEnumKeepsDeclaredOrder(t *testing.T) {
	shape := mustShape(t, mustLoad(t, IAM), "statusType")
	if shape.Type != "string" {
		t.Errorf("enum type = %q, want string", shape.Type)
	}
	if want := []string{"Active", "Inactive", "Expired"}; !reflect.DeepEqual(shape.Enum, want) {
		t.Errorf("statusType enum = %v, want %v", shape.Enum, want)
	}
}

func TestSmithyConstraints(t *testing.T) {
	model := mustLoad(t, IAM)
	request := mustShape(t, model, "CreateUserRequest")
	if want := []string{"UserName"}; !reflect.DeepEqual(request.Required, want) {
		t.Errorf("CreateUserRequest required = %v, want %v", request.Required, want)
	}
	userName := mustShape(t, model, request.Members["UserName"].Shape)
	if userName.Min == nil || *userName.Min != 1 || userName.Max == nil || *userName.Max != 64 {
		t.Errorf("user name length = %v..%v, want 1..64", userName.Min, userName.Max)
	}
	if userName.Pattern == "" {
		t.Error("user name pattern was not loaded")
	}
	maxItems := mustShape(t, model, "maxItemsType")
	if maxItems.Min == nil || *maxItems.Min != 1 || maxItems.Max == nil || *maxItems.Max != 1000 {
		t.Errorf("maxItemsType range = %v..%v, want 1..1000", maxItems.Min, maxItems.Max)
	}
}

func TestSmithyQueryResultWrapper(t *testing.T) {
	iam := mustLoad(t, IAM)
	deleteUser, _ := iam.Operation("DeleteUser")
	if deleteUser == nil || deleteUser.Output != nil {
		t.Errorf("DeleteUser output = %+v, want none for a Unit output", deleteUser)
	}
	if deleteUser.HTTP.Method != http.MethodPost || deleteUser.HTTP.RequestURI != "/" {
		t.Errorf("DeleteUser HTTP = %+v, want POST /", deleteUser.HTTP)
	}

	describe, _ := mustLoad(t, EC2).Operation("DescribeInstances")
	if describe == nil || describe.Output == nil || describe.Output.ResultWrapper != "" {
		t.Errorf("DescribeInstances output = %+v, want no wrapper for ec2", describe)
	}
}

func TestSmithyXMLNames(t *testing.T) {
	model := mustLoad(t, EC2)
	result := mustShape(t, model, "DescribeInstancesResult")
	reservations := result.Members["Reservations"]
	if reservations.LocationName != "reservationSet" || reservations.QueryName != "ReservationSet" {
		t.Errorf("Reservations ref = %+v, want reservationSet / ReservationSet", reservations)
	}
	if item := mustShape(t, model, reservations.Shape).Member; item == nil || item.LocationName != "item" {
		t.Errorf("ReservationList member = %+v, want item", item)
	}
}

func TestSmithyErrors(t *testing.T) {
	iamError := mustShape(t, mustLoad(t, IAM), "EntityAlreadyExistsException")
	if !iamError.Exception || iamError.Fault {
		t.Errorf("exception/fault = %v/%v, want true/false", iamError.Exception, iamError.Fault)
	}
	if want := (ErrorInfo{Code: "EntityAlreadyExists", HTTPStatusCode: 409, SenderFault: true}); iamError.Error == nil || *iamError.Error != want {
		t.Errorf("error = %+v, want %+v", iamError.Error, want)
	}

	// ACM declares awsQueryError codes for compatibility clients, but its
	// JSON wire code stays the shape name.
	acm := mustLoad(t, ACM)
	deleteCertificate, _ := acm.Operation("DeleteCertificate")
	codes := acm.OperationErrorCodes(deleteCertificate)
	if !strings.Contains(strings.Join(codes, ","), "AccessDeniedException") {
		t.Errorf("DeleteCertificate codes = %v, want AccessDeniedException", codes)
	}
}

func TestSmithyHTTPBindings(t *testing.T) {
	model := mustLoad(t, S3)
	getObject, _ := model.Operation("GetObject")
	if getObject.HTTP.Method != http.MethodGet || !strings.HasPrefix(getObject.HTTP.RequestURI, "/{Bucket}/{Key+}") || getObject.HTTP.ResponseCode != 200 {
		t.Errorf("GetObject HTTP = %+v", getObject.HTTP)
	}

	request := mustShape(t, model, getObject.Input.Shape)
	for member, want := range map[string][2]string{
		"Bucket":     {"uri", "Bucket"},
		"Range":      {"header", "Range"},
		"VersionId":  {"querystring", "versionId"},
		"PartNumber": {"querystring", "partNumber"},
	} {
		ref := request.Members[member]
		if ref.Location != want[0] || ref.LocationName != want[1] {
			t.Errorf("GetObjectRequest.%s binding = %s %s, want %s %s", member, ref.Location, ref.LocationName, want[0], want[1])
		}
	}

	output := mustShape(t, model, getObject.Output.Shape)
	if output.Payload != "Body" || !output.Members["Body"].Streaming {
		t.Errorf("GetObjectOutput payload = %q, Body = %+v", output.Payload, output.Members["Body"])
	}
	if metadata := output.Members["Metadata"]; metadata.Location != "headers" || metadata.LocationName != "x-amz-meta-" {
		t.Errorf("Metadata binding = %+v", metadata)
	}
	if expires := output.Members["LastModified"]; expires.LocationName != "Last-Modified" {
		t.Errorf("LastModified binding = %+v", expires)
	}
}

func TestSmithyPreludeTargets(t *testing.T) {
	model := mustLoad(t, ACM)
	account := mustShape(t, model, "AcmeAccount")
	ref := account.Members["AccountUrl"]
	if ref.Shape != "smithy.api#String" {
		t.Fatalf("AccountUrl target = %q, want the prelude String", ref.Shape)
	}
	if shape := mustShape(t, model, ref.Shape); shape.Type != "string" {
		t.Errorf("prelude String type = %q", shape.Type)
	}
}

func TestParseSmithyModelRejectsMalformedModels(t *testing.T) {
	const service = `"a.b#Svc": {"type": "service", "version": "1", "traits": {"aws.protocols#awsJson1_1": {}}}`
	const operation = `"a.b#Op": {"type": "operation", "input": {"target": "a.b#In"}}`
	const input = `"a.b#In": {"type": "structure", "members": {"X": {"target": "a.b#Missing"}}}`
	tests := map[string]struct {
		document string
		want     string
	}{
		"old smithy": {`{"smithy": "1.0", "shapes": {}}`, `want 2.x`},
		"no service": {`{"smithy": "2.0", "shapes": {` + operation + `}}`, "defines no service"},
		"two services": {
			`{"smithy": "2.0", "shapes": {` + service + `, "a.b#Other": {"type": "service"}}}`,
			"more than one service",
		},
		"no protocol":    {`{"smithy": "2.0", "shapes": {"a.b#Svc": {"type": "service"}}}`, "no supported protocol"},
		"unknown target": {`{"smithy": "2.0", "shapes": {` + service + `, ` + operation + `, ` + input + `}}`, "unknown target a.b#Missing"},
		"name collision": {
			`{"smithy": "2.0", "shapes": {` + service + `, "a.b#X": {"type": "string"}, "c.d#X": {"type": "string"}}}`,
			`share the local name "X"`,
		},
		"unsupported type": {
			`{"smithy": "2.0", "shapes": {` + service + `, ` + operation + `, "a.b#In": {"type": "mystery"}}}`,
			`unsupported shape type "mystery"`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSmithyModel("test", []byte(test.document))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}
