package utils

// GlobalAccountID is the root/system account ID.
const GlobalAccountID = "000000000000"

// AccountKey returns a KV key scoped to an account: "{accountID}.{resourceID}".
func AccountKey(accountID, resourceID string) string {
	return accountID + "." + resourceID
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
