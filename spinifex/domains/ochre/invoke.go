package ochre

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// resolveEndpointError maps a failed endpoint resolution onto the AWS code the
// daemon put on the wire, falling back to ServiceUnavailable for an un-coded
// transport failure.
func resolveEndpointError(err error) error {
	if code, ok := awserrors.ResolveErrorCode(err); ok {
		return errors.New(code)
	}
	return errors.New(awserrors.ErrorServiceUnavailableException)
}

// selfHostInvokeAdapter selects the InvokeAdapter for a self-host catalog
// entry by family: familyMeta serves through the Llama adapter (honouring a
// PT-account scope); any other family is refused rather than mis-served.
func selfHostInvokeAdapter(family string, endpointResolver EndpointResolver, ptAccountID string) (InvokeAdapter, error) {
	if family != familyMeta {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	if ptAccountID != "" {
		return newLlamaInvokeAdapterForAccount(endpointResolver, ptAccountID), nil
	}
	return newLlamaInvokeAdapter(endpointResolver), nil
}

// InvokeAdapter translates a Bedrock InvokeModel raw request body into a
// backend's native wire format and back, returning the response bytes
// verbatim with their content-type. Unlike Provider (Converse), the wire
// shape is per-family and is not unified by the gateway.
type InvokeAdapter interface {
	InvokeModel(ctx context.Context, modelID string, body []byte) (respBody []byte, contentType string, err error)
}

// InvokeModel routes modelID to its family adapter via the catalog. Unknown
// modelIds and unresolvable vendors return ResourceNotFoundException; an
// ungranted model, or a vendor with no resolvable credential, returns
// AccessDeniedException. Every exit records an InvocationRecord via the
// deferred closure. guardrailIdent/guardrailVersion come from the request's
// X-Amzn-Bedrock-Guardrail* headers; an empty guardrailIdent leaves behaviour
// byte-identical to a request with no guardrail at all.
func (rt *Router) InvokeModel(ctx context.Context, accountID, modelID string, body []byte, guardrailIdent, guardrailVersion string) (respBody []byte, contentType string, err error) {
	requestID := uuid.NewV4().String()
	start := time.Now()
	var backend string
	defer func() {
		httpStatus, code := recordOutcome(err)
		inputTokens, outputTokens, _ := extractTokenUsage(backend, respBody)
		rt.recorder.Record(ctx, InvocationRecord{
			RequestID:    requestID,
			AccountID:    accountID,
			ModelID:      modelID,
			Operation:    OperationInvokeModel,
			Backend:      backend,
			LatencyMs:    time.Since(start).Milliseconds(),
			HTTPStatus:   httpStatus,
			ErrorCode:    code,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			InputText:    string(body),
			OutputText:   string(respBody),
		})
	}()

	// Translate before resolve: a PT ARN is swapped for the commitment's own
	// (account, foundation model) here, before catalog lookup, access grant,
	// and endpoint resolution all act on it. Assigns the named err, so the
	// deferred closure records a rejected PT ARN the same as any other
	// failed invocation.
	var ptAccountID string
	ptAccountID, modelID, err = resolveInferenceTarget(ctx, accountID, modelID, rt.provisioned)
	if err != nil {
		return nil, "", err
	}

	// Assigns the named err, so the deferred closure records a denied
	// invocation the same as any other failed one.
	entry, err := grantedCatalogEntry(ctx, accountID, modelID, rt.access)
	if err != nil {
		return nil, "", err
	}
	backend = entry.Provider

	var a InvokeAdapter
	switch entry.Provider {
	case tierSelfHost:
		// Family validation first: a model this platform cannot serve at
		// all must fail ValidationException outright, not consume (or be
		// refused for) admission capacity it was never going to use.
		a, err = selfHostInvokeAdapter(entry.Family, rt.endpointResolver, ptAccountID)
		if err != nil {
			return nil, "", err
		}
		var release func()
		release, err = admitSelfHost(ctx, rt.provisioned, ptAccountID, modelID, entry)
		if err != nil {
			return nil, "", err
		}
		defer release()
	default:
		var key string
		key, err = rt.vendorKey(ctx, accountID, entry.Provider)
		if err != nil {
			return nil, "", err
		}
		a = newAnthropicInvokeAdapter(key)
	}

	// INPUT guardrail check happens after the adapter is resolved (matching
	// Router.Converse), before the backend is ever called. Text extraction is
	// per-family, since the body is opaque/family-native at this layer.
	if guardrailIdent != "" {
		var texts []string
		var extractOK bool
		texts, extractOK = extractInvokePromptTexts(backend, body)
		if !extractOK {
			err = errors.New(awserrors.ErrorValidationException)
			return nil, "", err
		}
		var blocked bool
		var message string
		blocked, message, _, _, err = enforceGuardrail(ctx, rt.guardrails, rt.embedder, accountID, guardrailIdent, guardrailVersion,
			bedrockruntime.GuardrailContentSourceInput, texts)
		if err != nil {
			return nil, "", err
		}
		if blocked {
			respBody, err = invokeGuardrailBlockedResponse(backend, modelID, message)
			if err != nil {
				return nil, "", err
			}
			return respBody, "application/json", nil
		}
	}

	respBody, contentType, err = a.InvokeModel(ctx, modelID, body)
	if err != nil || guardrailIdent == "" {
		return respBody, contentType, err
	}

	texts, extractOK := extractInvokeCompletionTexts(backend, respBody)
	if !extractOK {
		slog.Error("invoke: failed to extract completion text for guardrail OUTPUT check, blocking", "model", modelID, "backend", backend)
		view, verr := loadGuardrailView(ctx, rt.guardrails, accountID, guardrailIdent, guardrailVersion)
		if verr != nil {
			return nil, "", verr
		}
		respBody, err = invokeGuardrailBlockedResponse(backend, modelID, view.BlockedOutputsMessaging)
		if err != nil {
			return nil, "", err
		}
		return respBody, "application/json", nil
	}
	var blockedOut bool
	var messageOut string
	var redacted []string
	var gerr error
	blockedOut, messageOut, redacted, _, gerr = enforceGuardrail(ctx, rt.guardrails, rt.embedder, accountID, guardrailIdent, guardrailVersion,
		bedrockruntime.GuardrailContentSourceOutput, texts)
	if gerr != nil {
		err = gerr
		return nil, "", err
	}
	if blockedOut {
		respBody, err = invokeGuardrailBlockedCompletion(backend, respBody, messageOut)
	} else {
		respBody, err = invokeGuardrailRedactedCompletion(backend, respBody, redacted)
	}
	if err != nil {
		return nil, "", err
	}
	return respBody, contentType, nil
}

// InvokeModel is the bedrock-runtime InvokeModel entry point used by the
// gateway route table. resolver, endpointResolver, recorder, access and
// provisioned may be nil; NewRouter supplies no-op (and, for access,
// deny-all; for provisioned, PT-ARN-rejecting) fallbacks. guardrailIdent/
// guardrailVersion come from the request's X-Amzn-Bedrock-Guardrail* headers.
// embedder drives topicPolicy's semantic match; nil falls back to the
// literal matcher.
func InvokeModel(ctx context.Context, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver, recorder Recorder, access AccessResolver, provisioned *ProvisionedStore, guardrailIdent, guardrailVersion string, guardrails *GuardrailStore, embedder Embedder) ([]byte, string, error) {
	return NewRouter(resolver, endpointResolver, recorder, access, provisioned, guardrails, embedder).InvokeModel(ctx, accountID, modelID, body, guardrailIdent, guardrailVersion)
}

// InvokeStreamAdapter is the optional streaming capability an InvokeAdapter
// may implement. Both shipped families (Llama/vLLM, Anthropic) do;
// Router.InvokeModelWithResponseStream type-asserts the resolved InvokeAdapter to it rather
// than widening InvokeAdapter itself, mirroring ConverseStreamProvider.
type InvokeStreamAdapter interface {
	InvokeModelWithResponseStream(ctx context.Context, modelID string, body []byte) (invokeStreamSource, error)
}

// InvokeModelWithResponseStream routes modelID to its family adapter via the
// catalog, exactly like Router.InvokeModel — including the same
// translate-before-resolve treatment of a PT ARN via rt.provisioned — then
// requires the resolved adapter to also implement InvokeStreamAdapter.
// guardrailIdent/guardrailVersion come from the request's
// X-Amzn-Bedrock-Guardrail* headers; an empty guardrailIdent leaves behaviour
// byte-identical to a request with no guardrail at all. An INPUT block never
// opens the backend stream; OUTPUT enforcement wraps the resolved source for
// buffered/SYNC assessment at end-of-stream.
func (rt *Router) InvokeModelWithResponseStream(ctx context.Context, accountID, modelID string, body []byte, guardrailIdent, guardrailVersion string) (invokeStreamSource, error) {
	// Translate before resolve, exactly like Router.InvokeModel.
	ptAccountID, modelID, err := resolveInferenceTarget(ctx, accountID, modelID, rt.provisioned)
	if err != nil {
		return nil, err
	}

	entry, err := grantedCatalogEntry(ctx, accountID, modelID, rt.access)
	if err != nil {
		return nil, err
	}

	var a InvokeAdapter
	// selfHostRelease is non-nil only on the self-host branch below; every
	// return past this point on that branch must release the slot exactly
	// once, either explicitly here or via the slot-releasing source wrapper
	// on the success path.
	var selfHostRelease func()
	switch entry.Provider {
	case tierSelfHost:
		// Family validation first: a model this platform cannot serve at
		// all must fail ValidationException outright, not consume (or be
		// refused for) admission capacity it was never going to use.
		a, err = selfHostInvokeAdapter(entry.Family, rt.endpointResolver, ptAccountID)
		if err != nil {
			return nil, err
		}
		selfHostRelease, err = admitSelfHost(ctx, rt.provisioned, ptAccountID, modelID, entry)
		if err != nil {
			return nil, err
		}
	default:
		key, err := rt.vendorKey(ctx, accountID, entry.Provider)
		if err != nil {
			return nil, err
		}
		a = newAnthropicInvokeAdapter(key)
	}

	sa, ok := a.(InvokeStreamAdapter)
	if !ok {
		if selfHostRelease != nil {
			selfHostRelease()
		}
		return nil, errors.New(awserrors.ErrorValidationException)
	}

	if guardrailIdent != "" {
		texts, extractOK := extractInvokePromptTexts(entry.Provider, body)
		if !extractOK {
			if selfHostRelease != nil {
				selfHostRelease()
			}
			return nil, errors.New(awserrors.ErrorValidationException)
		}
		blocked, message, _, _, gerr := enforceGuardrail(ctx, rt.guardrails, rt.embedder, accountID, guardrailIdent, guardrailVersion,
			bedrockruntime.GuardrailContentSourceInput, texts)
		if gerr != nil {
			if selfHostRelease != nil {
				selfHostRelease()
			}
			return nil, gerr
		}
		if blocked {
			chunks, berr := buildGuardedInvokeStreamChunks(entry.Provider, message, true)
			if berr != nil {
				if selfHostRelease != nil {
					selfHostRelease()
				}
				return nil, berr
			}
			// A blocked INPUT never opens the backend stream, so the slot
			// releases immediately rather than living past this return.
			if selfHostRelease != nil {
				selfHostRelease()
			}
			return &blockedInvokeStreamSource{chunks: chunks}, nil
		}
	}

	src, err := sa.InvokeModelWithResponseStream(ctx, modelID, body)
	if err != nil {
		if selfHostRelease != nil {
			selfHostRelease()
		}
		return nil, err
	}
	if selfHostRelease != nil {
		src = newSlotReleasingInvokeSource(src, selfHostRelease)
	}
	if guardrailIdent != "" {
		src = newGuardrailInvokeStreamSource(src, rt.guardrails, rt.embedder, accountID, guardrailIdent, guardrailVersion, entry.Provider)
	}
	return src, nil
}
