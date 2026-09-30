// Package ebspolicy owns EC2 EBS product policy that the control plane exposes.
package ebspolicy

// VolumeTypeGP3 is the only EBS volume type this platform currently serves.
const VolumeTypeGP3 = "gp3"

const (
	// GP3 IOPS envelope (AWS): 3000 baseline on any size, up to 500 IOPS/GiB,
	// capped at 16000.
	DefaultGP3IOPS = 3000
	MaxGP3IOPS     = 16000
	GP3IOPSPerGiB  = 500

	// GP3 throughput envelope (AWS): 125 MiB/s baseline, 1000 MiB/s ceiling,
	// flat range independent of volume size.
	DefaultGP3Throughput = 125
	MaxGP3Throughput     = 1000
)
