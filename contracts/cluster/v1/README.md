# Cluster v1 contracts

This package owns the versioned NATS contracts for cluster-membership control
plane traffic. It is not an AWS tenant-facing API: it lets platform consumers
identify the daemon nodes currently responding to a cluster fan-out.

`NodesDiscoverSubject` retains the deployed `spinifex.nodes.discover` route.
`NodeDiscoverResponse.Node` is the node identifier used for responder-set
coverage. Do not change either incompatibly in this package; add a new version
when a wire change is required.
