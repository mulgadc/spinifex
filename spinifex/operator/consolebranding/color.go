package consolebranding

import "regexp"

var hexColorRE = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// validHex reports a #RRGGBB colour. The strict shape is what makes the
// generated stylesheet safe: no accepted value can carry CSS syntax.
func validHex(s string) bool {
	return hexColorRE.MatchString(s)
}
