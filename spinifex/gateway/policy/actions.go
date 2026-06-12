package policy

// IAMAction formats a service and action as the IAM policy string "service:ActionName".
func IAMAction(service, action string) string {
	return service + ":" + action
}
