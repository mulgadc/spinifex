package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
)

// decodeJSONBody is the generic gateway HTTP adapter for ECR JSON 1.1 action
// wrappers that are not yet direct awsapi handlers. ECR request semantics stay
// in the receiving action; this helper only turns a readable HTTP body into a
// Go request shape.
func decodeJSONBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return awsapi.MalformedBodyError()
	}
	return nil
}
