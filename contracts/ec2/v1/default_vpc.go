package ec2v1

// EnsureDefaultVpcSubject is the request/reply subject on which the daemon
// builds an account's default VPC and acknowledges, so the gateway and the
// daemon share the route without importing each other.
const EnsureDefaultVpcSubject = "ec2.EnsureDefaultVpc"
