package viperblock

// NBDTransport controls how Viperblockd exports a mounted volume.
type NBDTransport string

const (
	// NBDTransportSocket uses Unix domain sockets (faster, local only).
	NBDTransportSocket NBDTransport = "socket"
	// NBDTransportTCP uses TCP connections (required for remote/DPU scenarios).
	NBDTransportTCP NBDTransport = "tcp"
)
