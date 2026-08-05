package awsmodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

const ec2ErrorCatalogPath = "models/ec2/error-codes.json"

type ec2ErrorClass string

const (
	ec2ErrorClassClient ec2ErrorClass = "client"
	ec2ErrorClassServer ec2ErrorClass = "server"
)

type ec2ErrorCatalogFile struct {
	Source         string   `json:"source"`
	VerifiedOn     string   `json:"verifiedOn"`
	CommonClient   []string `json:"commonClient"`
	SpecificClient []string `json:"specificClient"`
	Server         []string `json:"server"`
}

type ec2ErrorCatalog struct {
	metadata ec2ErrorCatalogFile
	codes    map[string]ec2ErrorClass
}

var loadEC2ErrorCatalog = sync.OnceValues(func() (*ec2ErrorCatalog, error) {
	contents, err := modelFS.ReadFile(ec2ErrorCatalogPath)
	if err != nil {
		return nil, fmt.Errorf("awsmodel: read EC2 error catalog: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var file ec2ErrorCatalogFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("awsmodel: parse EC2 error catalog: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("awsmodel: parse EC2 error catalog: %w", err)
	}
	if file.Source == "" || file.VerifiedOn == "" {
		return nil, errors.New("awsmodel: EC2 error catalog requires source and verifiedOn")
	}

	catalog := &ec2ErrorCatalog{metadata: file, codes: make(map[string]ec2ErrorClass)}
	add := func(label string, values []string, class ec2ErrorClass) error {
		if !slices.IsSorted(values) {
			return fmt.Errorf("awsmodel: EC2 error catalog %s codes are not sorted", label)
		}
		for _, code := range values {
			if code == "" {
				return fmt.Errorf("awsmodel: EC2 error catalog %s contains an empty code", label)
			}
			if previous, exists := catalog.codes[code]; exists {
				return fmt.Errorf("awsmodel: EC2 error catalog code %q is duplicated (%s and %s)", code, previous, label)
			}
			catalog.codes[code] = class
		}
		return nil
	}
	if err := add("commonClient", file.CommonClient, ec2ErrorClassClient); err != nil {
		return nil, err
	}
	if err := add("specificClient", file.SpecificClient, ec2ErrorClassClient); err != nil {
		return nil, err
	}
	if err := add("server", file.Server, ec2ErrorClassServer); err != nil {
		return nil, err
	}
	return catalog, nil
})

// ValidateEC2ErrorResponse validates an EC2 Query API error envelope against
// the checked-in catalog curated from AWS's EC2 error-code reference. The AWS
// api-2.json model declares no operation errors, so this is intentionally a
// separate oracle.
func ValidateEC2ErrorResponse(status int, body []byte) ([]Violation, error) {
	root, err := parseXML(body)
	if err != nil {
		return nil, fmt.Errorf("awsmodel: decode EC2 error response: %w", err)
	}
	if root.name != "Response" {
		return nil, fmt.Errorf("awsmodel: decode EC2 error response: root element is %q, want %q", root.name, "Response")
	}
	errorsNode := directXMLChild(root, "Errors")
	if errorsNode == nil {
		return nil, errors.New("awsmodel: decode EC2 error response: Errors element is missing")
	}
	errorNode := directXMLChild(errorsNode, "Error")
	if errorNode == nil {
		return nil, errors.New("awsmodel: decode EC2 error response: Error element is missing")
	}
	code := directXMLText(errorNode, "Code")
	if code == "" {
		return nil, errors.New("awsmodel: decode EC2 error response: Code element is missing")
	}

	catalog, err := loadEC2ErrorCatalog()
	if err != nil {
		return nil, err
	}
	class, known := catalog.codes[code]
	if !known {
		return []Violation{{
			Rule:    RuleErrorCode,
			Path:    "$error.Code",
			Message: fmt.Sprintf("error code %q is not in the curated EC2 catalog", code),
		}}, nil
	}

	wantStatusClass := 4
	if class == ec2ErrorClassServer {
		wantStatusClass = 5
	}
	if status/100 != wantStatusClass {
		return []Violation{{
			Rule:    RuleHTTPStatus,
			Path:    "$response.status",
			Message: fmt.Sprintf("status %d is not in the %dxx class required for EC2 %s error %q", status, wantStatusClass, class, code),
		}}, nil
	}
	return nil, nil
}

func directXMLChild(node *xmlNode, name string) *xmlNode {
	for _, child := range node.children {
		if child.name == name {
			return child
		}
	}
	return nil
}

func directXMLText(node *xmlNode, name string) string {
	child := directXMLChild(node, name)
	if child == nil {
		return ""
	}
	return strings.TrimSpace(child.text.String())
}
