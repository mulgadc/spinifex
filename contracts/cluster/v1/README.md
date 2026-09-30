# Cluster v1 contracts

This package owns the versioned cluster control-plane wire contracts. It is
not an AWS tenant-facing API: it lets platform consumers identify responding
daemon nodes and inspect their health.

`NodesDiscoverSubject` retains the deployed `spinifex.nodes.discover` route.
`NodeDiscoverResponse.Node` is the node identifier used for responder-set
coverage. Do not change either incompatibly in this package; add a new version
when a wire change is required.

`NodeHealthSubject` retains the deployed per-node NATS route, while
`NodeHealthResponse` is also the daemon HTTPS `/health` representation. Do not
change their subject or JSON shape incompatibly in this package; add a new
version when a wire change is required.
