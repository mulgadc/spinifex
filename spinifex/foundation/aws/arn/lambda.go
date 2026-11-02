package arn

import "fmt"

// FormatLambdaFunction builds arn:aws:lambda:<region>:<account>:function:<name>.
func FormatLambdaFunction(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", region, accountID, name)
}
