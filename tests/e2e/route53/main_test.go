//go:build e2e

// Package route53 is the hosted-zone E2E suite. It drives northstar, the
// authoritative DNS server behind Route 53, in both single and multinode
// topologies, because every node serves every zone from the same bucket.
package route53
