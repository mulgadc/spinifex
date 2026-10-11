package identifiers

// GlobalAccountID is the root/system account ID.
const GlobalAccountID = "000000000000"

// DefaultAccountID returns the fixed, platform-reserved ID of the first
// human-facing account created during bootstrap (000000000001).
func DefaultAccountID() string {
	return "000000000001"
}

// DefaultAccountName returns the fixed name of the bootstrap account ("spinifex").
func DefaultAccountName() string {
	return "spinifex"
}

// IsAccountID checks if a string is a valid 12-digit AWS account ID.
func IsAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
