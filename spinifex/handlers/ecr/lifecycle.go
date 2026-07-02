package ecr

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// ErrInvalidLifecyclePolicy is returned when a lifecycle-policy document is
// malformed or violates the supported rule schema. The gateway maps it to
// InvalidParameterValue.
var ErrInvalidLifecyclePolicy = errors.New("ecr: invalid lifecycle policy")

// Lifecycle tag-status selectors and count types (AWS lifecycle-policy schema).
const (
	lifecycleTagStatusTagged   = "tagged"
	lifecycleTagStatusUntagged = "untagged"
	lifecycleTagStatusAny      = "any"

	lifecycleCountImageCountMoreThan = "imageCountMoreThan"
	lifecycleCountSinceImagePushed   = "sinceImagePushed"

	lifecycleCountUnitDays = "days"
	lifecycleActionExpire  = "expire"

	maxLifecycleRules = 100
)

// LifecyclePolicyDoc is the parsed lifecycle-policy document.
type LifecyclePolicyDoc struct {
	Rules []LifecycleRule `json:"rules"`
}

// LifecycleRule is one expiry rule. Lower RulePriority evaluates first.
type LifecycleRule struct {
	RulePriority int                `json:"rulePriority"`
	Description  string             `json:"description"`
	Selection    LifecycleSelection `json:"selection"`
	Action       LifecycleAction    `json:"action"`
}

// LifecycleSelection picks the images a rule acts on.
type LifecycleSelection struct {
	TagStatus     string   `json:"tagStatus"`
	TagPrefixList []string `json:"tagPrefixList"`
	CountType     string   `json:"countType"`
	CountUnit     string   `json:"countUnit"`
	CountNumber   int      `json:"countNumber"`
}

// LifecycleAction is the action a rule applies; only "expire" is supported.
type LifecycleAction struct {
	Type string `json:"type"`
}

// LifecycleImage is an image presented to the evaluation engine.
type LifecycleImage struct {
	Digest   string
	Tags     []string
	PushedAt time.Time
}

// LifecycleExpiry is an image the engine selected for expiry, annotated with the
// priority of the rule that claimed it.
type LifecycleExpiry struct {
	Digest       string
	Tags         []string
	PushedAt     time.Time
	RulePriority int
}

// ParseLifecyclePolicy decodes and validates a lifecycle-policy document. It
// returns ErrInvalidLifecyclePolicy on malformed JSON or an unsupported rule.
func ParseLifecyclePolicy(text []byte) (LifecyclePolicyDoc, error) {
	var doc LifecyclePolicyDoc
	dec := json.NewDecoder(strings.NewReader(string(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return LifecyclePolicyDoc{}, ErrInvalidLifecyclePolicy
	}
	if len(doc.Rules) == 0 || len(doc.Rules) > maxLifecycleRules {
		return LifecyclePolicyDoc{}, ErrInvalidLifecyclePolicy
	}
	seen := make(map[int]bool, len(doc.Rules))
	for _, rule := range doc.Rules {
		if err := validateLifecycleRule(rule, seen); err != nil {
			return LifecyclePolicyDoc{}, err
		}
	}
	return doc, nil
}

func validateLifecycleRule(rule LifecycleRule, seenPriority map[int]bool) error {
	if rule.RulePriority < 1 || seenPriority[rule.RulePriority] {
		return ErrInvalidLifecyclePolicy
	}
	seenPriority[rule.RulePriority] = true

	sel := rule.Selection
	switch sel.TagStatus {
	case lifecycleTagStatusTagged:
		if len(sel.TagPrefixList) == 0 {
			return ErrInvalidLifecyclePolicy
		}
	case lifecycleTagStatusUntagged, lifecycleTagStatusAny:
		if len(sel.TagPrefixList) > 0 {
			return ErrInvalidLifecyclePolicy
		}
	default:
		return ErrInvalidLifecyclePolicy
	}

	if sel.CountNumber < 1 {
		return ErrInvalidLifecyclePolicy
	}
	switch sel.CountType {
	case lifecycleCountSinceImagePushed:
		if sel.CountUnit != lifecycleCountUnitDays {
			return ErrInvalidLifecyclePolicy
		}
	case lifecycleCountImageCountMoreThan:
		if sel.CountUnit != "" {
			return ErrInvalidLifecyclePolicy
		}
	default:
		return ErrInvalidLifecyclePolicy
	}

	if rule.Action.Type != lifecycleActionExpire {
		return ErrInvalidLifecyclePolicy
	}
	return nil
}

// EvaluateLifecyclePolicy returns the images that the policy would expire, given
// the repo's current images and the evaluation time. Each image is claimed by at
// most one rule (lowest RulePriority first); no image is deleted here.
func EvaluateLifecyclePolicy(text []byte, images []LifecycleImage, now time.Time) ([]LifecycleExpiry, error) {
	doc, err := ParseLifecyclePolicy(text)
	if err != nil {
		return nil, err
	}

	rules := make([]LifecycleRule, len(doc.Rules))
	copy(rules, doc.Rules)
	sort.Slice(rules, func(i, j int) bool { return rules[i].RulePriority < rules[j].RulePriority })

	// Newest-first, digest-tiebreak: deterministic for imageCountMoreThan and the
	// result ordering.
	ordered := make([]LifecycleImage, len(images))
	copy(ordered, images)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].PushedAt.Equal(ordered[j].PushedAt) {
			return ordered[i].PushedAt.After(ordered[j].PushedAt)
		}
		return ordered[i].Digest < ordered[j].Digest
	})

	claimed := make(map[string]bool, len(ordered))
	var out []LifecycleExpiry
	for _, rule := range rules {
		var selected []LifecycleImage
		for _, img := range ordered {
			if !claimed[img.Digest] && matchesLifecycleSelection(img, rule.Selection) {
				selected = append(selected, img)
			}
		}
		for _, img := range expireFromSelection(selected, rule.Selection, now) {
			claimed[img.Digest] = true
			out = append(out, LifecycleExpiry{Digest: img.Digest, Tags: img.Tags, PushedAt: img.PushedAt, RulePriority: rule.RulePriority})
		}
	}
	return out, nil
}

// matchesLifecycleSelection reports whether an image is in a rule's tag scope.
func matchesLifecycleSelection(img LifecycleImage, sel LifecycleSelection) bool {
	switch sel.TagStatus {
	case lifecycleTagStatusUntagged:
		return len(img.Tags) == 0
	case lifecycleTagStatusAny:
		return true
	case lifecycleTagStatusTagged:
		if len(img.Tags) == 0 {
			return false
		}
		for _, tag := range img.Tags {
			for _, prefix := range sel.TagPrefixList {
				if strings.HasPrefix(tag, prefix) {
					return true
				}
			}
		}
		return false
	default:
		return false
	}
}

// expireFromSelection applies the count rule to the already-selected, newest-
// first images and returns the subset to expire.
func expireFromSelection(selected []LifecycleImage, sel LifecycleSelection, now time.Time) []LifecycleImage {
	switch sel.CountType {
	case lifecycleCountSinceImagePushed:
		cutoff := now.AddDate(0, 0, -sel.CountNumber)
		var expire []LifecycleImage
		for _, img := range selected {
			if img.PushedAt.Before(cutoff) {
				expire = append(expire, img)
			}
		}
		return expire
	case lifecycleCountImageCountMoreThan:
		if len(selected) <= sel.CountNumber {
			return nil
		}
		return selected[sel.CountNumber:]
	default:
		return nil
	}
}
