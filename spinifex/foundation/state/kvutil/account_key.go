package kvutil

// AccountKey returns the conventional account-scoped KV key:
// "{accountID}.{resourceID}".
func AccountKey(accountID, resourceID string) string {
	return accountID + "." + resourceID
}
