package ebsprovider

// Page returns one page of refs built from ids, and the token that resumes after
// it. ids must be sorted ascending; an empty next token means the last page,
// including when the page fits exactly.
func Page[T any](ids []string, startingToken string, pageSize int, ref func(id string) T) (page []T, next string) {
	var last string
	for _, id := range ids {
		if id <= startingToken {
			continue
		}
		// Checked before appending, so a token is only issued once an id beyond a
		// full page is known to exist. An exact fit ends the walk with no token.
		if len(page) == pageSize {
			return page, last
		}
		page = append(page, ref(id))
		last = id
	}
	return page, ""
}
