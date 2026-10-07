// Package utils holds the helpers still shared across Spinifex services: the
// default-VPC subject, the Viperblock master key loader and vpcd events.
package utils

// SubjectEnsureDefaultVpc is the request/reply subject on which the daemon
// builds an account's default VPC and acknowledges. It lives here rather than
// beside either party so the gateway does not import the daemon, nor the
// reverse.
const SubjectEnsureDefaultVpc = "ec2.EnsureDefaultVpc"
