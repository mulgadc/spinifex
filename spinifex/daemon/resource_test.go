package daemon

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

func TestCanAllocateCount(t *testing.T) {
	tests := []struct {
		name      string
		availVCPU int
		allocVCPU int
		availMem  float64
		allocMem  float64
		vCPUs     int64
		memMiB    int64
		maxCount  int
		want      int
	}{
		{
			name:      "exact fit single instance",
			availVCPU: 4, allocVCPU: 2,
			availMem: 8.0, allocMem: 4.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 10,
			want:     1,
		},
		{
			name:      "multiple instances fit",
			availVCPU: 16, allocVCPU: 0,
			availMem: 32.0, allocMem: 0.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 10,
			want:     8, // limited by CPU: 16/2 = 8, mem: 32/4 = 8
		},
		{
			name:      "CPU limited",
			availVCPU: 4, allocVCPU: 0,
			availMem: 64.0, allocMem: 0.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 10,
			want:     2, // CPU: 4/2=2, mem: 64/4=16 → min=2
		},
		{
			name:      "memory limited",
			availVCPU: 64, allocVCPU: 0,
			availMem: 8.0, allocMem: 0.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 10,
			want:     2, // CPU: 64/2=32, mem: 8/4=2 → min=2
		},
		{
			name:      "capped by maxCount",
			availVCPU: 64, allocVCPU: 0,
			availMem: 128.0, allocMem: 0.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 3,
			want:     3,
		},
		{
			name:      "zero remaining resources",
			availVCPU: 4, allocVCPU: 4,
			availMem: 8.0, allocMem: 8.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 5,
			want:     0,
		},
		{
			name:      "negative remaining (overallocated)",
			availVCPU: 4, allocVCPU: 6,
			availMem: 8.0, allocMem: 10.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 5,
			want:     0,
		},
		{
			name:      "zero vCPUs bypasses CPU check",
			availVCPU: 4, allocVCPU: 0,
			availMem: 16.0, allocMem: 0.0,
			vCPUs: 0, memMiB: 4096,
			maxCount: 5,
			want:     4, // CPU check skipped (maxCount=5), mem: 16/4=4 → min=4
		},
		{
			name:      "zero memory bypasses mem check",
			availVCPU: 8, allocVCPU: 0,
			availMem: 16.0, allocMem: 0.0,
			vCPUs: 2, memMiB: 0,
			maxCount: 5,
			want:     4, // CPU: 8/2=4, mem check skipped (maxCount=5) → min=4
		},
		{
			name:      "maxCount zero",
			availVCPU: 16, allocVCPU: 0,
			availMem: 32.0, allocMem: 0.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 0,
			want:     0,
		},
		{
			name:      "off by one CPU",
			availVCPU: 5, allocVCPU: 0,
			availMem: 64.0, allocMem: 0.0,
			vCPUs: 2, memMiB: 4096,
			maxCount: 10,
			want:     2, // 5/2 = 2 (integer division)
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := canAllocateCount(tc.availVCPU, tc.allocVCPU, tc.availMem, tc.allocMem, tc.vCPUs, tc.memMiB, tc.maxCount, 0, false)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCanAllocateCount_GPU(t *testing.T) {
	// Base resource params: plenty of CPU and memory, 1 GPU available.
	const (
		avCPU  = 32
		alCPU  = 0
		avMem  = 256.0
		alMem  = 0.0
		vCPUs  = 4 // g5.xlarge
		memMiB = 16384
	)

	// GPU available: normal CPU/mem-constrained allocation with GPU cap.
	got := canAllocateCount(avCPU, alCPU, avMem, alMem, vCPUs, memMiB, 10, 1, true)
	assert.Equal(t, 1, got, "with 1 GPU available, max 1 instance despite spare CPU/mem")

	// 3 GPUs available: cap is 3.
	got = canAllocateCount(avCPU, alCPU, avMem, alMem, vCPUs, memMiB, 10, 3, true)
	assert.Equal(t, 3, got, "with 3 GPUs, GPU pool caps the count")

	// 0 GPUs: must return 0 regardless of CPU/mem headroom.
	got = canAllocateCount(avCPU, alCPU, avMem, alMem, vCPUs, memMiB, 10, 0, true)
	assert.Equal(t, 0, got, "no GPUs available must return 0")

	// Non-GPU type is unaffected by availGPU=0.
	got = canAllocateCount(avCPU, alCPU, avMem, alMem, vCPUs, memMiB, 10, 0, false)
	assert.Equal(t, 8, got, "non-GPU type: CPU 32/4=8; memory and GPU not limiting")

	// maxCount caps GPU result.
	got = canAllocateCount(avCPU, alCPU, avMem, alMem, vCPUs, memMiB, 2, 5, true)
	assert.Equal(t, 2, got, "maxCount=2 caps GPU result")
}

func TestResourceStatsForType(t *testing.T) {
	tests := []struct {
		name       string
		remainVCPU int
		remainMem  float64
		it         *ec2.InstanceTypeInfo
		wantName   string
		wantVCPU   int
		wantMemGB  float64
		wantAvail  int
	}{
		{
			name:       "standard instance type",
			remainVCPU: 8,
			remainMem:  16.0,
			it: &ec2.InstanceTypeInfo{
				InstanceType: aws.String("t3.medium"),
				VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(2)},
				MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(4096)},
			},
			wantName:  "t3.medium",
			wantVCPU:  2,
			wantMemGB: 4.0,
			wantAvail: 4, // min(8/2=4, 16/4=4)
		},
		{
			name:       "CPU limited",
			remainVCPU: 2,
			remainMem:  64.0,
			it: &ec2.InstanceTypeInfo{
				InstanceType: aws.String("c5.xlarge"),
				VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(4)},
				MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(8192)},
			},
			wantName:  "c5.xlarge",
			wantVCPU:  4,
			wantMemGB: 8.0,
			wantAvail: 0, // CPU: 2/4=0
		},
		{
			name:       "nil instance type name",
			remainVCPU: 16,
			remainMem:  32.0,
			it: &ec2.InstanceTypeInfo{
				VCpuInfo:   &ec2.VCpuInfo{DefaultVCpus: aws.Int64(2)},
				MemoryInfo: &ec2.MemoryInfo{SizeInMiB: aws.Int64(2048)},
			},
			wantName:  "",
			wantVCPU:  2,
			wantMemGB: 2.0,
			wantAvail: 8, // min(16/2=8, 32/2=16)
		},
		{
			name:       "zero vCPU gives zero available",
			remainVCPU: 16,
			remainMem:  32.0,
			it: &ec2.InstanceTypeInfo{
				InstanceType: aws.String("broken"),
				VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(0)},
				MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(4096)},
			},
			wantName:  "broken",
			wantVCPU:  0,
			wantMemGB: 4.0,
			wantAvail: 0,
		},
		{
			name:       "nil VCpuInfo",
			remainVCPU: 16,
			remainMem:  32.0,
			it: &ec2.InstanceTypeInfo{
				InstanceType: aws.String("broken2"),
				MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(4096)},
			},
			wantName:  "broken2",
			wantVCPU:  0,
			wantMemGB: 4.0,
			wantAvail: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			typeCap := resourceStatsForType(tc.remainVCPU, tc.remainMem, tc.it)
			assert.Equal(t, tc.wantName, typeCap.Name)
			assert.Equal(t, tc.wantVCPU, typeCap.VCPU)
			assert.InDelta(t, tc.wantMemGB, typeCap.MemoryGB, 0.001)
			assert.Equal(t, tc.wantAvail, typeCap.Available)
		})
	}
}

func TestApplyHostReserve(t *testing.T) {
	tests := []struct {
		name       string
		host       hostReserve
		totalVCPU  int
		totalMemGB float64
		wantVCPU   int
		wantMem    float64
		wantErr    bool
	}{
		{
			name:       "normal host with default reserve",
			host:       defaultHostReserve,
			totalVCPU:  16,
			totalMemGB: 64.0,
			wantVCPU:   2,
			wantMem:    2.0,
		},
		{
			name:       "boundary host (3 vCPU, 2.5 GB)",
			host:       defaultHostReserve,
			totalVCPU:  3,
			totalMemGB: 2.5,
			wantVCPU:   2,
			wantMem:    2.0,
		},
		{
			name:       "too small: vCPU equals reserve",
			host:       defaultHostReserve,
			totalVCPU:  2,
			totalMemGB: 8.0,
			wantErr:    true,
		},
		{
			name:       "too small: vCPU below reserve",
			host:       defaultHostReserve,
			totalVCPU:  1,
			totalMemGB: 8.0,
			wantErr:    true,
		},
		{
			name:       "too small: mem at reserve threshold (no headroom)",
			host:       defaultHostReserve,
			totalVCPU:  8,
			totalMemGB: 2.0,
			wantErr:    true,
		},
		{
			name:       "too small: mem just under reserve+headroom",
			host:       defaultHostReserve,
			totalVCPU:  8,
			totalMemGB: 2.49,
			wantErr:    true,
		},
		{
			name:       "custom reserve passed through",
			host:       hostReserve{vCPU: 4, memGB: 8.0},
			totalVCPU:  16,
			totalMemGB: 64.0,
			wantVCPU:   4,
			wantMem:    8.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotVCPU, gotMem, err := applyHostReserve(tc.host, tc.totalVCPU, tc.totalMemGB)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.wantVCPU, gotVCPU)
			assert.InDelta(t, tc.wantMem, gotMem, 0.001)
		})
	}
}

func TestAllocateForLaunch(t *testing.T) {
	tests := []struct {
		name     string
		canAlloc int
		minCount int
		maxCount int
		want     int
		wantErr  bool
	}{
		{
			name:     "exact min equals max",
			canAlloc: 5, minCount: 5, maxCount: 5,
			want: 5,
		},
		{
			name:     "capacity exceeds max",
			canAlloc: 10, minCount: 1, maxCount: 5,
			want: 5,
		},
		{
			name:     "capacity between min and max",
			canAlloc: 3, minCount: 1, maxCount: 5,
			want: 3,
		},
		{
			name:     "capacity below min",
			canAlloc: 2, minCount: 3, maxCount: 5,
			wantErr: true,
		},
		{
			name:     "zero capacity",
			canAlloc: 0, minCount: 1, maxCount: 5,
			wantErr: true,
		},
		{
			name:     "zero min count always succeeds",
			canAlloc: 0, minCount: 0, maxCount: 5,
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := allocateForLaunch(tc.canAlloc, tc.minCount, tc.maxCount)
			if tc.wantErr {
				assert.ErrorIs(t, err, errInsufficientCapacity)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
