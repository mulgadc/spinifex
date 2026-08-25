package gateway_bedrock

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gateway/bodyscope"
)

// The resource a policy check evaluates against when the request names nothing
// in particular, or when the identifier cannot be resolved at gate time.
const anyResource = "*"

// Stands in for the id of a resource the request is about to create. It is a
// value, so a policy scoped to the resource type matches it where "*" would
// not; a Deny naming the stored id cannot fire, because it does not exist yet.
const anyID = "*"

// foundationModelResourceType is the ARN resource-type segment for a foundation
// model. AWS spells these ARNs with an empty account segment, because the model
// catalogue is not an account resource, and a policy written against AWS must
// match here.
const foundationModelResourceType = "foundation-model"

// Where an action's resource is named. Path parameters come from the route
// regex; body sources are read from the same JSON the handler unmarshals.
type resourceSource uint8

const (
	sourceAny resourceSource = iota
	sourceFoundationModelPath
	sourceInferenceTargetPath
	sourceGuardrailPath
	sourceNewGuardrail
	sourceProvisionedModelPath
	sourceNewProvisionedModel
	sourceProvisionedModelSourceBody
	sourceKnowledgeBasePath
	sourceNewKnowledgeBase
	sourceKnowledgeBaseBody
)

// bedrockScopes holds one table per service the Bedrock family serves. They
// share a package because they share the ARN builders and the id-or-ARN
// spelling every Bedrock surface accepts. Exhaustive by contract: a
// completeness test compares each table with its dispatch table in both
// directions, so an action cannot be added with a silent account-wide grant.
var bedrockScopes = map[string]map[string][]resourceSource{
	"bedrock": {
		// The model catalogue is not an account resource; the list is
		// account-level and the get names one model.
		"ListFoundationModels": {sourceAny},
		"GetFoundationModel":   {sourceFoundationModelPath},

		// The model-invocation logging configuration is account-level.
		"PutModelInvocationLoggingConfiguration":    {sourceAny},
		"GetModelInvocationLoggingConfiguration":    {sourceAny},
		"DeleteModelInvocationLoggingConfiguration": {sourceAny},

		// Provisioned throughput. A create evaluates the commitment it is about
		// to create and the foundation model it commits, matching AWS.
		"CreateProvisionedModelThroughput": {sourceNewProvisionedModel, sourceProvisionedModelSourceBody},
		"GetProvisionedModelThroughput":    {sourceProvisionedModelPath},
		"UpdateProvisionedModelThroughput": {sourceProvisionedModelPath},
		"DeleteProvisionedModelThroughput": {sourceProvisionedModelPath},
		"ListProvisionedModelThroughputs":  {sourceAny},

		// Guardrails.
		"CreateGuardrail":        {sourceNewGuardrail},
		"GetGuardrail":           {sourceGuardrailPath},
		"UpdateGuardrail":        {sourceGuardrailPath},
		"DeleteGuardrail":        {sourceGuardrailPath},
		"CreateGuardrailVersion": {sourceGuardrailPath},
		"ListGuardrails":         {sourceAny},
	},

	"bedrock-runtime": {
		// The path segment is a model id or a provisioned-throughput ARN, the
		// same translation resolveInferenceTarget performs downstream.
		"Converse":                      {sourceInferenceTargetPath},
		"ConverseStream":                {sourceInferenceTargetPath},
		"InvokeModel":                   {sourceInferenceTargetPath},
		"InvokeModelWithResponseStream": {sourceInferenceTargetPath},
		"ApplyGuardrail":                {sourceGuardrailPath},
	},

	// AWS's own DataSource shape carries no ARN, and the data-source and
	// ingestion-job actions are evaluated against the knowledge base, so both
	// path ids collapse to one resource.
	"bedrock-agent": {
		"CreateKnowledgeBase": {sourceNewKnowledgeBase},
		"ListKnowledgeBases":  {sourceAny},
		"GetKnowledgeBase":    {sourceKnowledgeBasePath},
		"DeleteKnowledgeBase": {sourceKnowledgeBasePath},
		"CreateDataSource":    {sourceKnowledgeBasePath},
		"ListDataSources":     {sourceKnowledgeBasePath},
		"GetDataSource":       {sourceKnowledgeBasePath},
		"DeleteDataSource":    {sourceKnowledgeBasePath},
		"StartIngestionJob":   {sourceKnowledgeBasePath},
		"ListIngestionJobs":   {sourceKnowledgeBasePath},
		"GetIngestionJob":     {sourceKnowledgeBasePath},
	},

	"bedrock-agent-runtime": {
		"Retrieve":            {sourceKnowledgeBasePath},
		"RetrieveAndGenerate": {sourceKnowledgeBaseBody},
	},
}

// HasScope reports whether action has an explicit scope-table entry under
// service.
func HasScope(service, action string) bool {
	_, ok := bedrockScopes[service][action]
	return ok
}

// ScopedActions returns every action represented in service's scope table.
func ScopedActions(service string) []string {
	scopes := bedrockScopes[service]
	actions := make([]string, 0, len(scopes))
	for action := range scopes {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}

// ResourceARNs resolves the resources a Bedrock-family request authorizes
// against from the path parameters and body the dispatcher already holds.
// params are the route captures, already percent-decoded.
func ResourceARNs(service, action, region, accountID string, params []string, body []byte) ([]string, error) {
	sources, ok := bedrockScopes[service][action]
	if !ok {
		slog.Error("bedrock authz: action is served but absent from the scope table", "service", service, "action", action)
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}

	scope, err := bodyscope.Parse(action, body)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	resources := make([]string, 0, len(sources))
	for _, source := range sources {
		resource, err := resolve(source, service, action, region, accountID, params, scope)
		if err != nil {
			return nil, err
		}
		// An unresolved member drops out rather than contributing "*", which no
		// scoped Allow can match and which would deny a call AWS permits.
		if resource == anyResource || slices.Contains(resources, resource) {
			continue
		}
		resources = append(resources, resource)
	}
	if len(resources) == 0 {
		return []string{anyResource}, nil
	}
	return resources, nil
}

func resolve(source resourceSource, service, action, region, accountID string, params []string, scope bodyscope.Scope) (string, error) {
	switch source {
	case sourceAny:
		return anyResource, nil

	case sourceFoundationModelPath:
		return foundationModelARN(region, param(params, 0)), nil

	case sourceInferenceTargetPath:
		return inferenceTargetARN(region, accountID, param(params, 0)), nil

	case sourceGuardrailPath:
		return guardrailARN(region, accountID, param(params, 0)), nil

	case sourceNewGuardrail:
		return FormatGuardrailARN(region, accountID, anyID), nil

	case sourceProvisionedModelPath:
		return provisionedModelARN(region, accountID, param(params, 0)), nil

	case sourceNewProvisionedModel:
		return FormatProvisionedModelARN(region, accountID, anyID), nil

	case sourceProvisionedModelSourceBody:
		return foundationModelARN(region, scope.String("modelId")), nil

	case sourceKnowledgeBasePath:
		return knowledgeBaseARN(region, accountID, param(params, 0)), nil

	case sourceNewKnowledgeBase:
		return FormatKnowledgeBaseARN(region, accountID, anyID), nil

	case sourceKnowledgeBaseBody:
		config, err := scope.Object("retrieveAndGenerateConfiguration")
		if err != nil {
			return "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		kb, err := config.Object("knowledgeBaseConfiguration")
		if err != nil {
			return "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		return knowledgeBaseARN(region, accountID, kb.String("knowledgeBaseId")), nil

	default:
		slog.Error("bedrock authz: unhandled resource source, failing closed",
			"service", service, "action", action, "source", source)
		return "", errors.New(awserrors.ErrorInternalError)
	}
}

// An absent identifier authorizes account-wide, so a malformed request stays
// the handler's validation fault rather than becoming an authorization failure.
func foundationModelARN(region, modelID string) string {
	if region == "" || modelID == "" {
		return anyResource
	}
	return fmt.Sprintf("arn:aws:bedrock:%s::%s/%s", region, foundationModelResourceType, modelID)
}

// inferenceTargetARN names what the caller addressed: a commitment when the
// modelId is a provisioned-throughput ARN, and the foundation model itself
// otherwise, matching the translation resolveInferenceTarget performs.
func inferenceTargetARN(region, accountID, modelID string) string {
	if !looksLikeProvisionedModelARN(modelID) {
		return foundationModelARN(region, modelID)
	}
	return provisionedModelARN(region, accountID, modelID)
}

// provisionedModelARN accepts the id-or-ARN spelling every PT op allows and
// re-anchors it on gw.Region and the caller's account. An ARN naming another
// account resolves account-wide: the handler rejects it, so a denial here would
// replace a validation fault with an authorization one.
func provisionedModelARN(region, accountID, idOrARN string) string {
	if region == "" || accountID == "" || idOrARN == "" {
		return anyResource
	}
	id, err := resolveProvisionedModelID(idOrARN, region, accountID)
	if err != nil || id == "" {
		return anyResource
	}
	return FormatProvisionedModelARN(region, accountID, id)
}

// guardrailARN accepts the id-or-ARN spelling every guardrail op allows,
// mirroring provisionedModelARN.
func guardrailARN(region, accountID, idOrARN string) string {
	if region == "" || accountID == "" || idOrARN == "" {
		return anyResource
	}
	id, err := resolveGuardrailID(idOrARN, region, accountID)
	if err != nil || id == "" {
		return anyResource
	}
	return FormatGuardrailARN(region, accountID, id)
}

func knowledgeBaseARN(region, accountID, id string) string {
	if region == "" || accountID == "" || id == "" {
		return anyResource
	}
	return FormatKnowledgeBaseARN(region, accountID, id)
}

func param(params []string, i int) string {
	if i >= len(params) {
		return ""
	}
	return params[i]
}
