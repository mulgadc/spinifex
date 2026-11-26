package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/rest"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

// bedrockRoute maps one HTTP method + chi path pattern to an AWS action and handler.
type bedrockRoute = rest.Route[bedrockRouteHandler]

// bedrockRouteHandler invokes a per-action bedrock (control-plane) gateway
// function. params holds the path params, PathUnescape'd. resolver
// is d.resolver(): the configured credential store, or a no-op
// fallback. loggingStore is d.loggingStore(). access is
// d.accessResolver(): the configured grant store, or a deny-all
// fallback. provisioned is d.provisionedStore(inv.Region). guardrails is
// d.guardrailStore(inv.Region).
type bedrockRouteHandler func(ctx context.Context, accountID string, params []string, body []byte, resolver ochre.CredentialResolver, loggingStore *ochre.LoggingConfigStore, access ochre.AccessResolver, provisioned *ochre.ProvisionedStore, guardrails *ochre.GuardrailStore) (any, error)

// bedrockRoutes is the dispatch table. Order is presentational: the router
// matches through a chi trie, which prefers a literal segment over a {param} one.
var bedrockRoutes = []bedrockRoute{
	{Method: "GET", Pattern: "/foundation-models", Action: "ListFoundationModels",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, resolver ochre.CredentialResolver, _ *ochre.LoggingConfigStore, access ochre.AccessResolver, _ *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			return ochre.ListFoundationModels(ctx, acct, resolver, access, new(bedrock.ListFoundationModelsInput))
		}},
	{Method: "GET", Pattern: "/foundation-models/{modelIdentifier}", Action: "GetFoundationModel",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, access ochre.AccessResolver, _ *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			return ochre.GetFoundationModel(ctx, acct, p[0], access)
		}},
	{Method: "PUT", Pattern: "/logging/modelinvocations", Action: "PutModelInvocationLoggingConfiguration",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, store *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			input := new(bedrock.PutModelInvocationLoggingConfigurationInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return ochre.PutModelInvocationLoggingConfiguration(ctx, acct, store, input)
		}},
	{Method: "GET", Pattern: "/logging/modelinvocations", Action: "GetModelInvocationLoggingConfiguration",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, store *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			return ochre.GetModelInvocationLoggingConfiguration(ctx, acct, store, new(bedrock.GetModelInvocationLoggingConfigurationInput))
		}},
	{Method: "DELETE", Pattern: "/logging/modelinvocations", Action: "DeleteModelInvocationLoggingConfiguration",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, store *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			return ochre.DeleteModelInvocationLoggingConfiguration(ctx, acct, store, new(bedrock.DeleteModelInvocationLoggingConfigurationInput))
		}},
	{Method: "POST", Pattern: "/provisioned-model-throughput", Action: "CreateProvisionedModelThroughput",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, provisioned *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			input := new(bedrock.CreateProvisionedModelThroughputInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return ochre.CreateProvisionedModelThroughput(ctx, acct, provisioned, input)
		}},
	{Method: "GET", Pattern: "/provisioned-model-throughput/{provisionedModelId}", Action: "GetProvisionedModelThroughput",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, provisioned *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			return ochre.GetProvisionedModelThroughput(ctx, acct, provisioned, &bedrock.GetProvisionedModelThroughputInput{ProvisionedModelId: aws.String(p[0])})
		}},
	{Method: "GET", Pattern: "/provisioned-model-throughputs", Action: "ListProvisionedModelThroughputs",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, provisioned *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			input, err := listProvisionedQuery(b)
			if err != nil {
				return nil, err
			}
			return ochre.ListProvisionedModelThroughputs(ctx, acct, provisioned, input)
		}},
	{Method: "PATCH", Pattern: "/provisioned-model-throughput/{provisionedModelId}", Action: "UpdateProvisionedModelThroughput",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, provisioned *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			input := new(bedrock.UpdateProvisionedModelThroughputInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.ProvisionedModelId = aws.String(p[0])
			return ochre.UpdateProvisionedModelThroughput(ctx, acct, provisioned, input)
		}},
	{Method: "DELETE", Pattern: "/provisioned-model-throughput/{provisionedModelId}", Action: "DeleteProvisionedModelThroughput",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, provisioned *ochre.ProvisionedStore, _ *ochre.GuardrailStore) (any, error) {
			return ochre.DeleteProvisionedModelThroughput(ctx, acct, provisioned, &bedrock.DeleteProvisionedModelThroughputInput{ProvisionedModelId: aws.String(p[0])})
		}},
	{Method: "POST", Pattern: "/guardrails", Action: "CreateGuardrail",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, guardrails *ochre.GuardrailStore) (any, error) {
			input := new(bedrock.CreateGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return ochre.CreateGuardrail(ctx, acct, guardrails, input)
		}},
	{Method: "GET", Pattern: "/guardrails/{guardrailIdentifier}", Action: "GetGuardrail",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, guardrails *ochre.GuardrailStore) (any, error) {
			input := new(bedrock.GetGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			return ochre.GetGuardrail(ctx, acct, guardrails, input)
		}},
	{Method: "GET", Pattern: "/guardrails", Action: "ListGuardrails",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, guardrails *ochre.GuardrailStore) (any, error) {
			q, err := listQuery(b)
			if err != nil {
				return nil, err
			}
			input := &bedrock.ListGuardrailsInput{GuardrailIdentifier: q.str("guardrailIdentifier"), NextToken: q.str("nextToken")}
			if input.MaxResults, err = q.int64("maxResults"); err != nil {
				return nil, err
			}
			return ochre.ListGuardrails(ctx, acct, guardrails, input)
		}},
	{Method: "PUT", Pattern: "/guardrails/{guardrailIdentifier}", Action: "UpdateGuardrail",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, guardrails *ochre.GuardrailStore) (any, error) {
			input := new(bedrock.UpdateGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			return ochre.UpdateGuardrail(ctx, acct, guardrails, input)
		}},
	{Method: "DELETE", Pattern: "/guardrails/{guardrailIdentifier}", Action: "DeleteGuardrail",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, guardrails *ochre.GuardrailStore) (any, error) {
			input := new(bedrock.DeleteGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			return ochre.DeleteGuardrail(ctx, acct, guardrails, input)
		}},
	{Method: "POST", Pattern: "/guardrails/{guardrailIdentifier}", Action: "CreateGuardrailVersion",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ ochre.CredentialResolver, _ *ochre.LoggingConfigStore, _ ochre.AccessResolver, _ *ochre.ProvisionedStore, guardrails *ochre.GuardrailStore) (any, error) {
			input := new(bedrock.CreateGuardrailVersionInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			return ochre.CreateGuardrailVersion(ctx, acct, guardrails, input)
		}},
}

// bedrockRouter matches an escaped request path against bedrockRoutes.
var bedrockRouter = rest.NewRouter("bedrock", bedrockRoutes)

// Bedrock_Request dispatches bedrock (control-plane) REST-JSON requests:
// resolves method+path to an action, reads the body, calls the handler, and
// serialises the output as JSON.
func (d Deps) bedrock(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	action, params, handler, ok := bedrockRouter.Lookup(r.Method, r.URL.EscapedPath())
	if !ok {
		slog.DebugContext(r.Context(), "bedrock: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Hoisted above the policy check because the resolver builds ARNs from it.
	accountID := inv.AccountID
	if accountID == "" {
		slog.ErrorContext(r.Context(), "Bedrock_Request: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	body, err := dispatch.ReadBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "Bedrock_Request: failed to read body", "err", err)
		return err
	}

	// Some REST-JSON actions carry their non-path inputs as singular query
	// params with an empty body (e.g. GetGuardrail's guardrailVersion arrives
	// as GET /guardrails/{id}?guardrailVersion=1). Only folds when the body is
	// empty so it never shadows a real payload, mirroring EKS_Request's
	// own query fold for its (repeated-value) tagKeys case.
	if len(body) == 0 {
		if q := r.URL.Query(); len(q) > 0 {
			flat := make(map[string]string, len(q))
			for k, v := range q {
				if len(v) > 0 {
					flat[k] = v[0]
				}
			}
			if qb, err := json.Marshal(flat); err == nil {
				body = qb
			}
		}
	}

	// After the body read and the query fold: CreateProvisionedModelThroughput
	// names the foundation model it commits there rather than in the path.
	resources, err := ResourceARNs("bedrock", action, inv.Region, accountID, params, body)
	if err != nil {
		return err
	}
	if err := inv.Authorize("bedrock", action, resources, nil); err != nil {
		return err
	}

	output, err := handler(r.Context(), accountID, params, body, d.resolver(), d.loggingStore(), d.accessResolver(), d.provisionedStore(inv.Region), d.guardrailStore(inv.Region))
	if err != nil {
		return err
	}

	envelope.WriteJSONResult(w, output)
	return nil
}

// bedrockQuery is a list call's query string, folded into the body by bedrock.
type bedrockQuery map[string]string

// listQuery decodes the folded query string of a list call.
func listQuery(b []byte) (bedrockQuery, error) {
	q := bedrockQuery{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &q); err != nil {
			return nil, errors.New(awserrors.ErrorValidationException)
		}
	}
	return q, nil
}

func (q bedrockQuery) str(name string) *string {
	if v, ok := q[name]; ok {
		return aws.String(v)
	}
	return nil
}

func (q bedrockQuery) int64(name string) (*int64, error) {
	v, ok := q[name]
	if !ok {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException, "%s must be an integer", name)
	}
	return &n, nil
}

func (q bedrockQuery) time(name string) (*time.Time, error) {
	v, ok := q[name]
	if !ok {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException, "%s must be an ISO 8601 timestamp", name)
	}
	return &t, nil
}

// listProvisionedQuery builds ListProvisionedModelThroughputs' input from its
// query string.
func listProvisionedQuery(b []byte) (*bedrock.ListProvisionedModelThroughputsInput, error) {
	q, err := listQuery(b)
	if err != nil {
		return nil, err
	}
	input := &bedrock.ListProvisionedModelThroughputsInput{
		ModelArnEquals: q.str("modelArnEquals"),
		NameContains:   q.str("nameContains"),
		NextToken:      q.str("nextToken"),
		SortBy:         q.str("sortBy"),
		SortOrder:      q.str("sortOrder"),
		StatusEquals:   q.str("statusEquals"),
	}
	if input.MaxResults, err = q.int64("maxResults"); err != nil {
		return nil, err
	}
	if input.CreationTimeAfter, err = q.time("creationTimeAfter"); err != nil {
		return nil, err
	}
	if input.CreationTimeBefore, err = q.time("creationTimeBefore"); err != nil {
		return nil, err
	}
	return input, nil
}
