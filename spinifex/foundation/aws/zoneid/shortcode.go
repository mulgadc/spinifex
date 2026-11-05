// Package zoneid resolves AWS-shaped availability-zone IDs (apse2-az1) for a
// Region and pins them in a cluster KV record so a later change to the table,
// the derivation rule or the operator override never reassigns an ID.
package zoneid

import (
	"fmt"
	"regexp"
	"strings"
)

var shortCodePattern = regexp.MustCompile(`^[a-z0-9]{2,8}$`)

// maxShortCodeLen is the longest code the pattern allows.
const maxShortCodeLen = 8

// ValidShortCode reports whether s is acceptable as a Region short code.
func ValidShortCode(s string) bool { return shortCodePattern.MatchString(s) }

// awsShortCodes is AWS's published Region to AZ ID prefix table.
var awsShortCodes = map[string]string{
	"us-east-1": "use1", "us-east-2": "use2", "us-west-1": "usw1", "us-west-2": "usw2",
	"ca-central-1": "cac1", "ca-west-1": "caw1", "mx-central-1": "mxc1",
	"af-south-1": "afs1",
	"ap-east-1":  "ape1", "ap-east-2": "ape2",
	"ap-northeast-1": "apne1", "ap-northeast-2": "apne2", "ap-northeast-3": "apne3",
	"ap-south-1": "aps1", "ap-south-2": "aps2",
	"ap-southeast-1": "apse1", "ap-southeast-2": "apse2", "ap-southeast-3": "apse3",
	"ap-southeast-4": "apse4", "ap-southeast-5": "apse5", "ap-southeast-6": "apse6",
	"ap-southeast-7": "apse7",
	"eu-central-1":   "euc1", "eu-central-2": "euc2", "eu-north-1": "eun1",
	"eu-south-1": "eus1", "eu-south-2": "eus2",
	"eu-west-1": "euw1", "eu-west-2": "euw2", "eu-west-3": "euw3",
	"il-central-1": "ilc1", "me-central-1": "mec1", "me-south-1": "mes1",
	"sa-east-1": "sae1",
}

var compassCompounds = map[string]string{
	"northeast": "ne", "northwest": "nw", "southeast": "se", "southwest": "sw",
}

// ShortCodeFor returns the short code for region: the operator override when
// set, else AWS's published code, else a derived one. It is a pure function;
// the pinned record, not this result, is what every read uses.
func ShortCodeFor(region, override string) (string, error) {
	if override != "" {
		if !ValidShortCode(override) {
			return "", fmt.Errorf("zoneid: az_short_code %q must match [a-z0-9]{2,8}", override)
		}
		return override, nil
	}
	region = strings.ToLower(strings.TrimSpace(region))
	if code, ok := awsShortCodes[region]; ok {
		return code, nil
	}
	return derive(region)
}

// derive builds a stable, parseable code: the first two characters of the
// first segment, one initial per later word (two for a compass compound), then
// the trailing number or 1.
func derive(region string) (string, error) {
	var letters strings.Builder
	digits := "1"
	for i, seg := range strings.Split(region, "-") {
		seg = alnum(seg)
		if seg == "" {
			continue
		}
		switch {
		case isNumber(seg):
			digits = seg
		case i == 0:
			letters.WriteString(seg[:min(2, len(seg))])
		default:
			if c, ok := compassCompounds[seg]; ok {
				letters.WriteString(c)
			} else {
				letters.WriteString(seg[:1])
			}
		}
	}
	if letters.Len() == 0 {
		return "", fmt.Errorf("zoneid: cannot derive a short code from region %q", region)
	}
	l := letters.String()
	if room := maxShortCodeLen - len(digits); len(l) > room {
		l = l[:max(room, 0)]
	}
	if code := l + digits; ValidShortCode(code) {
		return code, nil
	}
	return "", fmt.Errorf("zoneid: cannot derive a short code from region %q", region)
}

func alnum(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, s)
}

func isNumber(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
