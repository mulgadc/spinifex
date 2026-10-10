package progress

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		name string
		b    uint64
		want string
	}{
		{"Zero", 0, "0 B"},
		{"Bytes", 512, "512 B"},
		{"KiB", 1024, "1.0 KiB"},
		{"KiB fractional", 1536, "1.5 KiB"},
		{"MiB", 1048576, "1.0 MiB"},
		{"GiB", 1073741824, "1.0 GiB"},
		{"Large GiB", 5368709120, "5.0 GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HumanBytes(tt.b))
		})
	}
}

func TestProgressWriter(t *testing.T) {
	var total int
	pw := progressWriter(func(n int) {
		total += n
	})

	n, err := pw.Write([]byte("hello"))
	assert.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, 5, total)

	n, err = pw.Write([]byte("world!"))
	assert.NoError(t, err)
	assert.Equal(t, 6, n)
	assert.Equal(t, 11, total)
}
