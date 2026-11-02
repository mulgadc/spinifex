package sts

// ServiceTrusts reports whether trustDoc lets service assume the role on behalf
// of sourceAccount, with the same deny-first evaluation AssumeRole uses. A nil
// error means trusted; a refusal is AccessDenied.
func ServiceTrusts(trustDoc, service, sourceAccount string) error {
	return evalTrustPolicyFor(trustDoc, "", sourceAccount, []string{service})
}
