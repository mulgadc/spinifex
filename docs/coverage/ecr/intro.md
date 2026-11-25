### Layer transfer and replication are stubs

`BatchCheckLayerAvailability`, `InitiateLayerUpload`, `UploadLayerPart`, `CompleteLayerUpload` and `GetDownloadUrlForLayer` are stubs.
`docker` and other OCI clients push and pull over the `/v2/` endpoint on the same host, after `docker login` with the token from `GetAuthorizationToken`.

Registry replication is a stub, because a deployment has no second region to replicate to.
