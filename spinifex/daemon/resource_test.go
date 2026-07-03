package daemon

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestCanAllocateLocked_MIGvsWholeGPU(t *testing.T) {
	gpuInfo := &ec2.GpuInfo{
		Gpus: []*ec2.GpuDeviceInfo{{
			Count:        aws.Int64(1),
			Manufacturer: aws.String("NVIDIA"),
			Name:         aws.String("MIG 1g.10gb"),
			MemoryInfo:   &ec2.GpuDeviceMemoryInfo{SizeInMiB: aws.Int64(10240)},
		}},
		TotalGpuMemoryInMiB: aws.Int64(10240),
	}

	// Host with 8 schedulable vCPUs and 32 GiB schedulable memory (after reserve).
	rm := &ResourceManager{
		hostVCPU:     10,
		hostMemGB:    34.0,
		reservedVCPU: 2,
		reservedMem:  2.0,
	}

	migType := &ec2.InstanceTypeInfo{
		InstanceType: aws.String("mig.1g.10gb"),
		VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(4)},
		MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(32 * 1024)},
		GpuInfo:      gpuInfo,
	}
	wholeGPUType := &ec2.InstanceTypeInfo{
		InstanceType: aws.String("g7e.4xlarge"),
		VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(16)},
		MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(128 * 1024)},
		GpuInfo:      gpuInfo,
	}

	// MIG: exactly one slice fits (4 vCPUs, 32 GiB on an 8 vCPU / 32 GiB host).
	assert.Equal(t, 1, rm.canAllocateLocked(migType, 10), "MIG: one slice fits")

	// MIG: a second slice would exceed host resources — must be gated.
	rm.allocatedVCPU = 4
	rm.allocatedMem = 32.0
	assert.Equal(t, 0, rm.canAllocateLocked(migType, 10), "MIG: second slice rejected when resources exhausted")

	// Whole-GPU: resource check bypassed — count returned regardless of headroom.
	assert.Equal(t, 10, rm.canAllocateLocked(wholeGPUType, 10), "whole-GPU: bypasses CPU/mem check")
}

func TestResolveHostReserve(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantVCPU int
		wantMem  float64
	}{
		{
			name:     "no env returns default",
			env:      map[string]string{},
			wantVCPU: defaultHostReserve.vCPU,
			wantMem:  defaultHostReserve.memGB,
		},
		{
			name:     "vCPU override",
			env:      map[string]string{"SPINIFEX_RESERVED_VCPU": "1"},
			wantVCPU: 1,
			wantMem:  defaultHostReserve.memGB,
		},
		{
			name:     "memory override",
			env:      map[string]string{"SPINIFEX_RESERVED_MEM_GB": "0.5"},
			wantVCPU: defaultHostReserve.vCPU,
			wantMem:  0.5,
		},
		{
			name: "both overrides",
			env: map[string]string{
				"SPINIFEX_RESERVED_VCPU":   "0",
				"SPINIFEX_RESERVED_MEM_GB": "1.5",
			},
			wantVCPU: 0,
			wantMem:  1.5,
		},
		{
			name:     "invalid vCPU falls back to default",
			env:      map[string]string{"SPINIFEX_RESERVED_VCPU": "garbage"},
			wantVCPU: defaultHostReserve.vCPU,
			wantMem:  defaultHostReserve.memGB,
		},
		{
			name:     "negative vCPU falls back to default",
			env:      map[string]string{"SPINIFEX_RESERVED_VCPU": "-1"},
			wantVCPU: defaultHostReserve.vCPU,
			wantMem:  defaultHostReserve.memGB,
		},
		{
			name:     "invalid memory falls back to default",
			env:      map[string]string{"SPINIFEX_RESERVED_MEM_GB": "not-a-number"},
			wantVCPU: defaultHostReserve.vCPU,
			wantMem:  defaultHostReserve.memGB,
		},
		{
			name:     "negative memory falls back to default",
			env:      map[string]string{"SPINIFEX_RESERVED_MEM_GB": "-0.5"},
			wantVCPU: defaultHostReserve.vCPU,
			wantMem:  defaultHostReserve.memGB,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveHostReserve(func(k string) string { return tc.env[k] })
			assert.Equal(t, tc.wantVCPU, got.vCPU)
			assert.InDelta(t, tc.wantMem, got.memGB, 0.001)
		})
	}
}

func TestResolveHostVCPU(t *testing.T) {
	const detected = 12
	tests := []struct {
		name string
		env  string
		want int
	}{
		{name: "no env returns detected", env: "", want: detected},
		{name: "valid override", env: "8", want: 8},
		{name: "non-numeric falls back to detected", env: "garbage", want: detected},
		{name: "zero falls back to detected", env: "0", want: detected},
		{name: "negative falls back to detected", env: "-4", want: detected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveHostVCPU(func(string) string { return tc.env }, detected)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLiveMemCount(t *testing.T) {
	tests := []struct {
		name                             string
		n                                int
		availMemGB, reservedMemGB, memGB float64
		want                             int
	}{
		{"live looser than accounting keeps n", 3, 20, 2, 4, 3},
		{"live tighter clamps below n", 5, 10, 2, 4, 2},      // (10-2)/4 = 2
		{"live exactly meets n", 2, 10, 2, 4, 2},             // (10-2)/4 = 2
		{"headroom below one guest yields 0", 3, 5, 2, 4, 0}, // (5-2)/4 = 0
		{"available below reserve yields 0", 3, 1, 2, 4, 0},  // negative headroom
		{"zero count stays zero", 0, 100, 2, 4, 0},
		{"zero memGB is a no-op (returns n)", 3, 1, 2, 0, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveMemCount(tc.n, tc.availMemGB, tc.reservedMemGB, tc.memGB)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseMemAvailableKB(t *testing.T) {
	meminfo := "MemTotal:       16384000 kB\n" +
		"MemFree:         1048576 kB\n" +
		"MemAvailable:    8388608 kB\n" +
		"Buffers:          204800 kB\n"
	tests := []struct {
		name   string
		data   string
		wantKB int64
		wantOK bool
	}{
		{"present", meminfo, 8388608, true},
		{"absent", "MemTotal: 16384000 kB\nMemFree: 1048576 kB\n", 0, false},
		{"malformed value", "MemAvailable:    notanumber kB\n", 0, false},
		{"empty", "", 0, false},
		{"first match wins", "MemAvailable: 100 kB\nMemAvailable: 200 kB\n", 100, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kb, ok := parseMemAvailableKB([]byte(tc.data))
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantKB, kb)
		})
	}
}

func TestLiveMemAdmissionEnabled(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"", true},
		{"1", true},
		{"true", true},
		{"anything", true},
		{"0", false},
		{"false", false},
		{"off", false},
		{"no", false},
		{" OFF ", false},
		{"FALSE", false},
	}
	for _, tc := range tests {
		t.Run("env="+tc.env, func(t *testing.T) {
			got := liveMemAdmissionEnabled(func(string) string { return tc.env })
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLiveMemGate(t *testing.T) {
	it := &ec2.InstanceTypeInfo{
		InstanceType: aws.String("t3.medium"),
		MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(4096)}, // 4 GB
	}

	t.Run("nil reader is a no-op", func(t *testing.T) {
		rm := &ResourceManager{reservedMem: 2}
		assert.Equal(t, 5, rm.liveMemGate(5, it))
	})

	t.Run("read failure fails open", func(t *testing.T) {
		rm := &ResourceManager{reservedMem: 2, readMemAvailableGB: func() (float64, bool) { return 0, false }}
		assert.Equal(t, 5, rm.liveMemGate(5, it))
	})

	t.Run("live availability clamps below accounting", func(t *testing.T) {
		// MemAvailable 11 GB, reserve 2 -> 9 GB headroom / 4 GB = 2 guests.
		rm := &ResourceManager{reservedMem: 2, readMemAvailableGB: func() (float64, bool) { return 11, true }}
		assert.Equal(t, 2, rm.liveMemGate(5, it))
	})

	t.Run("host below reserve refuses all", func(t *testing.T) {
		rm := &ResourceManager{reservedMem: 2, readMemAvailableGB: func() (float64, bool) { return 1, true }}
		assert.Equal(t, 0, rm.liveMemGate(5, it))
	})

	t.Run("zero count short-circuits before read", func(t *testing.T) {
		called := false
		rm := &ResourceManager{reservedMem: 2, readMemAvailableGB: func() (float64, bool) { called = true; return 100, true }}
		assert.Equal(t, 0, rm.liveMemGate(0, it))
		assert.False(t, called, "reader must not be called when n<=0")
	})
}

func TestHostUnderMemoryPressure(t *testing.T) {
	t.Run("nil reader fails open", func(t *testing.T) {
		rm := &ResourceManager{reservedMem: 2}
		assert.False(t, rm.HostUnderMemoryPressure())
	})

	t.Run("read failure fails open", func(t *testing.T) {
		rm := &ResourceManager{reservedMem: 2, readMemAvailableGB: func() (float64, bool) { return 0, false }}
		assert.False(t, rm.HostUnderMemoryPressure())
	})

	t.Run("available above reserve is healthy", func(t *testing.T) {
		rm := &ResourceManager{reservedMem: 2, readMemAvailableGB: func() (float64, bool) { return 4, true }}
		assert.False(t, rm.HostUnderMemoryPressure())
	})

	t.Run("available below reserve is pressure", func(t *testing.T) {
		rm := &ResourceManager{reservedMem: 2, readMemAvailableGB: func() (float64, bool) { return 1, true }}
		assert.True(t, rm.HostUnderMemoryPressure())
	})
}

func TestResolveNbdkitCharge(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		main, aux := resolveNbdkitCharge(func(string) string { return "" })
		assert.Equal(t, defaultNbdkitMainMiB, main)
		assert.Equal(t, defaultNbdkitAuxMiB, aux)
	})

	t.Run("env overrides both", func(t *testing.T) {
		env := map[string]string{"SPINIFEX_NBDKIT_MAIN_MIB": "1024", "SPINIFEX_NBDKIT_AUX_MIB": "0"}
		main, aux := resolveNbdkitCharge(func(k string) string { return env[k] })
		assert.Equal(t, 1024, main)
		assert.Equal(t, 0, aux)
	})

	t.Run("invalid and negative values ignored", func(t *testing.T) {
		env := map[string]string{"SPINIFEX_NBDKIT_MAIN_MIB": "nope", "SPINIFEX_NBDKIT_AUX_MIB": "-50"}
		main, aux := resolveNbdkitCharge(func(k string) string { return env[k] })
		assert.Equal(t, defaultNbdkitMainMiB, main)
		assert.Equal(t, defaultNbdkitAuxMiB, aux)
	})
}

func TestNbdkitChargeMiB(t *testing.T) {
	// Default layout: 1 main + 2 aux at the measured per-class figures.
	assert.Equal(t, int64(768+2*96), nbdkitChargeMiB(1, 2, 768, 96))
	// Aux-only / main-only decompose linearly.
	assert.Equal(t, int64(192), nbdkitChargeMiB(0, 2, 768, 96))
	assert.Equal(t, int64(768), nbdkitChargeMiB(1, 0, 768, 96))
	// Zeroed charges (struct-literal RM in other tests) cost nothing.
	assert.Equal(t, int64(0), nbdkitChargeMiB(1, 2, 0, 0))
}

// TestRG6_FullCostCharge asserts RG-6: an instance's admission charge is its
// guest -m PLUS one nbdkit process per volume (main 768 MiB, aux 96 MiB over the
// default 1-main+2-aux layout), and an allocate/deallocate round-trip restores
// the exact baseline with no drift.
func TestRG6_FullCostCharge(t *testing.T) {
	rm, err := NewResourceManager(nil, nil, nil)
	require.NoError(t, err)

	require.Equal(t, defaultNbdkitMainMiB, rm.nbdkitMainMiB)
	require.Equal(t, defaultNbdkitAuxMiB, rm.nbdkitAuxMiB)

	it := &ec2.InstanceTypeInfo{
		InstanceType: aws.String("t3.medium"),
		VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(2)},
		MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(4096)},
	}

	wantCharge := int64(4096) + nbdkitChargeMiB(defaultMainVolumes, defaultAuxVolumes, rm.nbdkitMainMiB, rm.nbdkitAuxMiB)
	assert.Equal(t, wantCharge, rm.instanceMemChargeMiB(it),
		"RG-6: charge must be guest -m + nbdkit per volume, not guest -m alone")
	assert.Greater(t, rm.instanceMemChargeMiB(it), instanceTypeMemoryMiB(it),
		"RG-6: nbdkit must add to the charge — bare guest -m undercounts and overcommits")

	// Round-trip: allocate then deallocate restores the baseline exactly.
	rm.mu.Lock()
	rm.hostVCPU, rm.hostMemGB = 64, 256
	rm.reservedVCPU, rm.reservedMem = 0, 0
	rm.allocatedVCPU, rm.allocatedMem = 0, 0
	rm.readMemAvailableGB = nil // isolate accounting from the live gate
	rm.mu.Unlock()

	require.NoError(t, rm.allocate(it))
	assert.InDelta(t, float64(wantCharge)/1024.0, rm.allocatedMem, 1e-9,
		"RG-6: allocate must charge guest + nbdkit")

	rm.deallocate(it)
	assert.InDelta(t, 0.0, rm.allocatedMem, 1e-9,
		"RG-6: deallocate must restore the baseline — no accounting drift")
}
