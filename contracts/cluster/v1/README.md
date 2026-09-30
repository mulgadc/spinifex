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

`NodeVMsSubject` retains the deployed `spinifex.node.vms` fan-out route.
`NodeVMsResponse` is the VM inventory reply used by the SPX operator CLI and
by EKS host lookup. It is distinct from node status and capacity reporting:
do not add scheduling or physical-GPU inventory merely because those callers
also inspect nodes. Do not change its subject or JSON shape incompatibly; add
a new version when a wire change is required.

`NodeStatusSubject` retains the deployed `spinifex.node.status` fan-out route.
`NodeStatusResponse` carries daemon-reported status, schedulable capacity and
physical-GPU inventory for the SPX operator CLI and EC2/EKS placement callers.
It is an observation contract, not the owner of capacity, scheduling or GPU
allocation policy. Do not change its subject or JSON shape incompatibly; add
a new version when a wire change is required.
