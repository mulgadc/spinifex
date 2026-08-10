package gateway_bedrock

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
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

// scaffoldTopicPolicyAssessment is scaffoldContentPolicyAssessment's sibling
// for denied topics: no classifier means every configured topic evaluates to
// NONE rather than being matched against the text.
func scaffoldTopicPolicyAssessment(cfg *bedrock.GuardrailTopicPolicyConfig) *bedrockruntime.GuardrailTopicPolicyAssessment {
	if cfg == nil {
		return nil
	}
	return &bedrockruntime.GuardrailTopicPolicyAssessment{Topics: []*bedrockruntime.GuardrailTopic{}}
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

// applyGuardrailPolicies is the pure filter engine: it evaluates view's
// policies over texts for source (INPUT or OUTPUT) and returns the
// aws-sdk-go bedrockruntime shape's pieces. wordPolicy and
// sensitiveInformationPolicy are enforced; content/topic/contextualGrounding
// evaluate to NONE (see the scaffold* helpers). A BLOCK anywhere makes the
// overall action GUARDRAIL_INTERVENED and short-circuits redaction, since the
// caller substitutes the guardrail's blocked messaging instead of the text.
// source is accepted (not yet branched on) for parity with AWS's
// per-source assessment shape; the deterministic policies apply identically
// to INPUT and OUTPUT text until a content-policy classifier needs to tell
// them apart via InputStrength/OutputStrength.
func applyGuardrailPolicies(view guardrailView, texts []string, source string) (string, []*bedrockruntime.GuardrailAssessment, []string, *bedrockruntime.GuardrailUsage) {
	_ = source

	wordAssessment, wordBlocked := assessWordPolicy(view.WordPolicy, texts)
	piiAssessment, piiBlocked := assessSensitiveInformationPolicy(view.SensitiveInformationPolicy, texts)

	action := bedrockruntime.GuardrailActionNone
	outputs := texts
	switch {
	case wordBlocked || piiBlocked:
		action = bedrockruntime.GuardrailActionGuardrailIntervened
	case view.SensitiveInformationPolicy != nil:
		outputs = redactSensitiveInformation(view.SensitiveInformationPolicy, texts)
	}

	assessment := &bedrockruntime.GuardrailAssessment{
		WordPolicy:                 wordAssessment,
		SensitiveInformationPolicy: piiAssessment,
		ContentPolicy:              scaffoldContentPolicyAssessment(view.ContentPolicy),
		TopicPolicy:                scaffoldTopicPolicyAssessment(view.TopicPolicy),
		ContextualGroundingPolicy:  scaffoldContextualGroundingPolicyAssessment(view.ContextualGroundingPolicy),
	}

	return action, []*bedrockruntime.GuardrailAssessment{assessment}, outputs, guardrailUsage(view, texts)
}
