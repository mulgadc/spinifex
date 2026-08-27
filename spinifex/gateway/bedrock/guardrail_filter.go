package gateway_bedrock

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// piiPattern binds one AWS PII entity type to the regex that detects it.
// Ordered (not a map) so assessment output is deterministic across runs.
type piiPattern struct {
	entityType string
	pattern    *regexp.Regexp
}

// builtinPIIPatterns is the regex-detectable PII entity subset a guardrail
// can enforce without a classifier. Entities like NAME or ADDRESS need a
// model and are out of scope: a guardrail configuring one of those simply
// never matches here, the same as any other unsupported entity type.
var builtinPIIPatterns = []piiPattern{
	{bedrock.GuardrailPiiEntityTypeEmail, regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)},
	{bedrock.GuardrailPiiEntityTypePhone, regexp.MustCompile(`(?:\+?1[-.\s])?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`)},
	{bedrock.GuardrailPiiEntityTypeUsSocialSecurityNumber, regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{bedrock.GuardrailPiiEntityTypeCreditDebitCardNumber, regexp.MustCompile(`\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{1,4}\b`)},
	{bedrock.GuardrailPiiEntityTypeIpAddress, regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b|\b(?:[0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}\b`)},
	{bedrock.GuardrailPiiEntityTypeUrl, regexp.MustCompile(`\bhttps?://[^\s<>"]+`)},
	{bedrock.GuardrailPiiEntityTypeAwsAccessKey, regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{bedrock.GuardrailPiiEntityTypeAwsSecretKey, regexp.MustCompile(`\b[A-Za-z0-9+/]{40}\b`)},
}

// builtinPIIPattern looks up the detector regex for an AWS PII entity type,
// or nil when the type has no deterministic (regex-only) detector.
func builtinPIIPattern(entityType string) *regexp.Regexp {
	for _, p := range builtinPIIPatterns {
		if p.entityType == entityType {
			return p.pattern
		}
	}
	return nil
}

// managedProfanityWords is a small starter seed for the guardrail's
// PROFANITY managed word list. A real deployment would back this with a
// maintained wordlist; this is deliberately minimal.
var managedProfanityWords = []string{
	"damn", "hell", "crap", "bastard", "bitch", "asshole", "bullshit", "shit", "fuck",
}

// wordBoundaryPattern compiles a case-insensitive, whole-word regex for word,
// so a blocklist entry like "hell" never matches inside "hello".
func wordBoundaryPattern(word string) (*regexp.Regexp, error) {
	return regexp.Compile(`(?i)\b` + regexp.QuoteMeta(word) + `\b`)
}

// matchesWord reports whether word appears, whole-word and case-insensitive,
// in any of texts.
func matchesWord(texts []string, word string) bool {
	re, err := wordBoundaryPattern(word)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(texts, re.MatchString)
}

// assessWordPolicy checks texts against cfg's custom blocklist and managed
// profanity list. Every match is a block: wordPolicy has no ANONYMIZE action
// in AWS's own shape, only BLOCKED.
func assessWordPolicy(cfg *bedrock.GuardrailWordPolicyConfig, texts []string) (*bedrockruntime.GuardrailWordPolicyAssessment, bool) {
	if cfg == nil {
		return nil, false
	}

	blocked := false
	customWords := []*bedrockruntime.GuardrailCustomWord{}
	for _, w := range cfg.WordsConfig {
		if w == nil {
			continue
		}
		word := aws.StringValue(w.Text)
		if word == "" || !matchesWord(texts, word) {
			continue
		}
		customWords = append(customWords, &bedrockruntime.GuardrailCustomWord{
			Action: aws.String(bedrockruntime.GuardrailWordPolicyActionBlocked),
			Match:  aws.String(word),
		})
		blocked = true
	}

	managedWords := []*bedrockruntime.GuardrailManagedWord{}
	for _, m := range cfg.ManagedWordListsConfig {
		if m == nil || aws.StringValue(m.Type) != bedrock.GuardrailManagedWordsTypeProfanity {
			continue
		}
		for _, word := range managedProfanityWords {
			if !matchesWord(texts, word) {
				continue
			}
			managedWords = append(managedWords, &bedrockruntime.GuardrailManagedWord{
				Action: aws.String(bedrockruntime.GuardrailWordPolicyActionBlocked),
				Match:  aws.String(word),
				Type:   aws.String(bedrockruntime.GuardrailManagedWordTypeProfanity),
			})
			blocked = true
		}
	}

	return &bedrockruntime.GuardrailWordPolicyAssessment{CustomWords: customWords, ManagedWordLists: managedWords}, blocked
}

// assessSensitiveInformationPolicy scans texts for every PII entity type and
// custom regex cfg configures, recording one filter entry per match. A BLOCK
// action match sets blocked; an ANONYMIZE match is recorded but left for
// redactSensitiveInformation to apply once the caller knows nothing blocked.
func assessSensitiveInformationPolicy(cfg *bedrock.GuardrailSensitiveInformationPolicyConfig, texts []string) (*bedrockruntime.GuardrailSensitiveInformationPolicyAssessment, bool) {
	if cfg == nil {
		return nil, false
	}

	blocked := false
	piiEntities := []*bedrockruntime.GuardrailPiiEntityFilter{}
	for _, e := range cfg.PiiEntitiesConfig {
		if e == nil {
			continue
		}
		entityType := aws.StringValue(e.Type)
		pattern := builtinPIIPattern(entityType)
		if pattern == nil {
			continue
		}
		respAction := bedrockruntime.GuardrailSensitiveInformationPolicyActionAnonymized
		if aws.StringValue(e.Action) == bedrock.GuardrailSensitiveInformationActionBlock {
			respAction = bedrockruntime.GuardrailSensitiveInformationPolicyActionBlocked
		}
		for _, t := range texts {
			for _, match := range pattern.FindAllString(t, -1) {
				if respAction == bedrockruntime.GuardrailSensitiveInformationPolicyActionBlocked {
					blocked = true
				}
				piiEntities = append(piiEntities, &bedrockruntime.GuardrailPiiEntityFilter{
					Action: aws.String(respAction),
					Match:  aws.String(match),
					Type:   aws.String(entityType),
				})
			}
		}
	}

	regexes := []*bedrockruntime.GuardrailRegexFilter{}
	for _, r := range cfg.RegexesConfig {
		if r == nil {
			continue
		}
		re, err := regexp.Compile(aws.StringValue(r.Pattern))
		if err != nil {
			continue
		}
		respAction := bedrockruntime.GuardrailSensitiveInformationPolicyActionAnonymized
		if aws.StringValue(r.Action) == bedrock.GuardrailSensitiveInformationActionBlock {
			respAction = bedrockruntime.GuardrailSensitiveInformationPolicyActionBlocked
		}
		for _, t := range texts {
			for _, match := range re.FindAllString(t, -1) {
				if respAction == bedrockruntime.GuardrailSensitiveInformationPolicyActionBlocked {
					blocked = true
				}
				regexes = append(regexes, &bedrockruntime.GuardrailRegexFilter{
					Action: aws.String(respAction),
					Match:  aws.String(match),
					Name:   r.Name,
					Regex:  r.Pattern,
				})
			}
		}
	}

	return &bedrockruntime.GuardrailSensitiveInformationPolicyAssessment{PiiEntities: piiEntities, Regexes: regexes}, blocked
}

// redactSensitiveInformation replaces every ANONYMIZE-configured entity or
// custom-regex match with a sentinel ({ENTITY_TYPE} for a PII entity, or
// {NAME} for a custom regex). Only called once nothing in the guardrail's
// policies blocked the request outright.
func redactSensitiveInformation(cfg *bedrock.GuardrailSensitiveInformationPolicyConfig, texts []string) []string {
	out := make([]string, len(texts))
	copy(out, texts)

	for _, e := range cfg.PiiEntitiesConfig {
		if e == nil || aws.StringValue(e.Action) != bedrock.GuardrailSensitiveInformationActionAnonymize {
			continue
		}
		pattern := builtinPIIPattern(aws.StringValue(e.Type))
		if pattern == nil {
			continue
		}
		sentinel := fmt.Sprintf("{%s}", aws.StringValue(e.Type))
		for i, t := range out {
			out[i] = pattern.ReplaceAllString(t, sentinel)
		}
	}

	for _, r := range cfg.RegexesConfig {
		if r == nil || aws.StringValue(r.Action) != bedrock.GuardrailSensitiveInformationActionAnonymize {
			continue
		}
		re, err := regexp.Compile(aws.StringValue(r.Pattern))
		if err != nil {
			continue
		}
		sentinel := fmt.Sprintf("{%s}", aws.StringValue(r.Name))
		for i, t := range out {
			out[i] = re.ReplaceAllString(t, sentinel)
		}
	}

	return out
}

// scaffoldContentPolicyAssessment records that a configured content policy
// was evaluated with no filters triggered: v1 has no classifier to score
// hate/insults/sexual/violence/misconduct/prompt-attack, so it always
// evaluates to NONE. A later classifier-backed policy fills this branch in
// rather than replacing it.
func scaffoldContentPolicyAssessment(cfg *bedrock.GuardrailContentPolicyConfig) *bedrockruntime.GuardrailContentPolicyAssessment {
	if cfg == nil {
		return nil
	}
	return &bedrockruntime.GuardrailContentPolicyAssessment{Filters: []*bedrockruntime.GuardrailContentFilter{}}
}

// literalTopicHit is the deterministic exact-match check: topic's Name and
// each of its Examples are matched via matchesWord's word-boundary
// semantics. The long-form Definition is never matched literally — it reads
// as prose, not a phrase, and substring-matching it produces erratic hits.
// This always runs regardless of semantic availability, so an exact phrase
// hit is never lost to an embedder outage.
func literalTopicHit(topic *bedrock.GuardrailTopicConfig, texts []string) bool {
	phrases := []string{}
	if name := aws.StringValue(topic.Name); name != "" {
		phrases = append(phrases, name)
	}
	for _, ex := range topic.Examples {
		if example := aws.StringValue(ex); example != "" {
			phrases = append(phrases, example)
		}
	}
	return slices.ContainsFunc(phrases, func(phrase string) bool { return matchesWord(texts, phrase) })
}

// assessTopicPolicy checks texts against cfg's denied topics. A topic
// blocks on literalTopicHit's exact-match, or when any input text's
// embedding reaches topicSimilarityThreshold cosine similarity against any
// of the topic's Name/Definition/Examples phrase vectors (Definition is
// finally used here, unlike the literal path). embedder == nil is a
// deliberately literal-only deployment (tests, or no embedder wired) and is
// not an error. embedder != nil whose Embed call fails means the policy
// could not be semantically evaluated: if literal matching already blocked
// the request that block wins outright, otherwise this returns an error so
// the caller fails closed instead of passing unverified content through.
func assessTopicPolicy(ctx context.Context, embedder Embedder, cfg *bedrock.GuardrailTopicPolicyConfig, texts []string) (*bedrockruntime.GuardrailTopicPolicyAssessment, bool, error) {
	if cfg == nil {
		return nil, false, nil
	}

	var textVectors [][]float32
	semanticOK := false
	unverified := false
	if len(cfg.TopicsConfig) > 0 {
		vectors, err := embedGuardrailTexts(ctx, embedder, DefaultEmbeddingModel, texts)
		switch {
		case err != nil:
			unverified = true
		case vectors != nil:
			textVectors = vectors
			semanticOK = true
		}
	}

	blocked := false
	topics := []*bedrockruntime.GuardrailTopic{}
	for _, topic := range cfg.TopicsConfig {
		if topic == nil {
			continue
		}

		hit := literalTopicHit(topic, texts)
		if !hit && semanticOK {
			vectors, err := topicVectors(ctx, embedder, DefaultEmbeddingModel, topic)
			switch {
			case err != nil:
				unverified = true
			case vectors != nil:
				hit = topicSemanticHit(textVectors, vectors, topicSimilarityThreshold(topic))
			}
		}
		if !hit {
			continue
		}

		topics = append(topics, &bedrockruntime.GuardrailTopic{
			Name:   topic.Name,
			Type:   topic.Type,
			Action: aws.String(bedrockruntime.GuardrailTopicPolicyActionBlocked),
		})
		blocked = true
	}

	assessment := &bedrockruntime.GuardrailTopicPolicyAssessment{Topics: topics}
	switch {
	case blocked:
		return assessment, true, nil
	case unverified:
		return assessment, false, errors.New(awserrors.ErrorServiceUnavailableException)
	default:
		return assessment, false, nil
	}
}

// scaffoldContextualGroundingPolicyAssessment is
// scaffoldContentPolicyAssessment's sibling for grounding/relevance: without
// a source to ground against and a classifier to score it, it evaluates to
// NONE.
func scaffoldContextualGroundingPolicyAssessment(cfg *bedrock.GuardrailContextualGroundingPolicyConfig) *bedrockruntime.GuardrailContextualGroundingPolicyAssessment {
	if cfg == nil {
		return nil
	}
	return &bedrockruntime.GuardrailContextualGroundingPolicyAssessment{Filters: []*bedrockruntime.GuardrailContextualGroundingFilter{}}
}

// guardrailUsage reports policy units processed as len(texts) for every
// policy view actually configures, and zero for one it doesn't: a cheap,
// deterministic stand-in for AWS's real usage metering.
func guardrailUsage(view guardrailView, texts []string) *bedrockruntime.GuardrailUsage {
	n := aws.Int64(int64(len(texts)))
	zero := aws.Int64(0)
	usage := &bedrockruntime.GuardrailUsage{
		WordPolicyUnits:                     zero,
		SensitiveInformationPolicyUnits:     zero,
		SensitiveInformationPolicyFreeUnits: aws.Int64(0),
		ContentPolicyUnits:                  zero,
		TopicPolicyUnits:                    zero,
		ContextualGroundingPolicyUnits:      zero,
	}
	if view.WordPolicy != nil {
		usage.WordPolicyUnits = n
	}
	if view.SensitiveInformationPolicy != nil {
		usage.SensitiveInformationPolicyUnits = n
	}
	if view.ContentPolicy != nil {
		usage.ContentPolicyUnits = n
	}
	if view.TopicPolicy != nil {
		usage.TopicPolicyUnits = n
	}
	if view.ContextualGroundingPolicy != nil {
		usage.ContextualGroundingPolicyUnits = n
	}
	return usage
}

// applyGuardrailPolicies is the filter engine: it evaluates view's policies
// over texts for source (INPUT or OUTPUT) and returns the aws-sdk-go
// bedrockruntime shape's pieces. wordPolicy and sensitiveInformationPolicy
// are enforced deterministically; topicPolicy adds embedding-similarity
// scoring over the literal match (assessTopicPolicy, using embedder);
// content/contextualGrounding evaluate to NONE (see the scaffold* helpers,
// which need a classifier). A BLOCK anywhere makes the overall action
// GUARDRAIL_INTERVENED and short-circuits redaction, since the caller
// substitutes the guardrail's blocked messaging instead of the text. A
// non-nil error means assessTopicPolicy could not semantically verify texts
// and nothing else blocked outright: the caller must fail the request
// rather than use the zero-value action/assessments returned alongside it.
// source is accepted (not yet branched on) for parity with AWS's per-source
// assessment shape; the deterministic policies apply identically to INPUT
// and OUTPUT text until a content-policy classifier needs to tell them apart
// via InputStrength/OutputStrength.
func applyGuardrailPolicies(ctx context.Context, embedder Embedder, view guardrailView, texts []string, source string) (string, []*bedrockruntime.GuardrailAssessment, []string, *bedrockruntime.GuardrailUsage, error) {
	_ = source

	wordAssessment, wordBlocked := assessWordPolicy(view.WordPolicy, texts)
	piiAssessment, piiBlocked := assessSensitiveInformationPolicy(view.SensitiveInformationPolicy, texts)
	topicAssessment, topicBlocked, topicErr := assessTopicPolicy(ctx, embedder, view.TopicPolicy, texts)
	if topicErr != nil {
		return "", nil, nil, nil, topicErr
	}

	action := bedrockruntime.GuardrailActionNone
	outputs := texts
	switch {
	case wordBlocked || piiBlocked || topicBlocked:
		action = bedrockruntime.GuardrailActionGuardrailIntervened
	case view.SensitiveInformationPolicy != nil:
		outputs = redactSensitiveInformation(view.SensitiveInformationPolicy, texts)
	}

	assessment := &bedrockruntime.GuardrailAssessment{
		WordPolicy:                 wordAssessment,
		SensitiveInformationPolicy: piiAssessment,
		ContentPolicy:              scaffoldContentPolicyAssessment(view.ContentPolicy),
		TopicPolicy:                topicAssessment,
		ContextualGroundingPolicy:  scaffoldContextualGroundingPolicyAssessment(view.ContextualGroundingPolicy),
	}

	return action, []*bedrockruntime.GuardrailAssessment{assessment}, outputs, guardrailUsage(view, texts), nil
}
