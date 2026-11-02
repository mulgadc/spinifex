package arn

import "fmt"

// FormatSQSQueue builds arn:aws:sqs:<region>:<account>:<name>.
func FormatSQSQueue(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, accountID, name)
}
