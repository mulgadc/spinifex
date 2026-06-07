package daemon

import "testing"

func TestParsePhysicalCores(t *testing.T) {
	tests := []struct {
		name    string
		cpuinfo string
		want    int
		wantOK  bool
	}{
		{
			name: "hyperthreaded single socket: 2 cores / 4 threads",
			cpuinfo: "processor\t: 0\nphysical id\t: 0\ncore id\t: 0\n\n" +
				"processor\t: 1\nphysical id\t: 0\ncore id\t: 1\n\n" +
				"processor\t: 2\nphysical id\t: 0\ncore id\t: 0\n\n" +
				"processor\t: 3\nphysical id\t: 0\ncore id\t: 1\n",
			want:   2,
			wantOK: true,
		},
		{
			name: "hybrid: P-cores share core ids across HT, E-cores distinct",
			// 2 P-cores (core id 0,1) each 2 threads + 2 E-cores (core id 16,17).
			cpuinfo: "processor\t: 0\nphysical id\t: 0\ncore id\t: 0\n\n" +
				"processor\t: 1\nphysical id\t: 0\ncore id\t: 0\n\n" +
				"processor\t: 2\nphysical id\t: 0\ncore id\t: 1\n\n" +
				"processor\t: 3\nphysical id\t: 0\ncore id\t: 1\n\n" +
				"processor\t: 4\nphysical id\t: 0\ncore id\t: 16\n\n" +
				"processor\t: 5\nphysical id\t: 0\ncore id\t: 17\n",
			want:   4,
			wantOK: true,
		},
		{
			name: "dual socket: 2 sockets x 1 core, same core id",
			cpuinfo: "processor\t: 0\nphysical id\t: 0\ncore id\t: 0\n\n" +
				"processor\t: 1\nphysical id\t: 1\ncore id\t: 0\n",
			want:   2,
			wantOK: true,
		},
		{
			name:    "no topology fields (e.g. ARM/container) falls back",
			cpuinfo: "processor\t: 0\nvendor_id\t: GenuineIntel\n\nprocessor\t: 1\nvendor_id\t: GenuineIntel\n",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "empty",
			cpuinfo: "",
			want:    0,
			wantOK:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePhysicalCores([]byte(tt.cpuinfo))
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("parsePhysicalCores() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
