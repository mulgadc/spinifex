package ebsprovider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeExportValidate(t *testing.T) {
	nbd := &NBDExport{NBDURI: "nbd+unix:///?socket=/run/vol-1.sock"}
	block := &BlockExport{DevicePath: "/dev/sdb", SizeBytes: 10 << 30}

	tests := []struct {
		name    string
		export  VolumeExport
		wantErr string
	}{
		{name: "valid nbd", export: VolumeExport{Kind: ExportKindNBD, NBD: nbd}},
		{name: "valid block", export: VolumeExport{Kind: ExportKindBlock, Block: block}},
		{name: "empty kind", export: VolumeExport{NBD: nbd}, wantErr: `unknown export kind ""`},
		{name: "unknown kind", export: VolumeExport{Kind: "iscsi", NBD: nbd}, wantErr: `unknown export kind "iscsi"`},
		{name: "nbd kind without nbd variant", export: VolumeExport{Kind: ExportKindNBD}, wantErr: "nbd export has no nbd details"},
		{name: "nbd kind carrying only block variant", export: VolumeExport{Kind: ExportKindNBD, Block: block}, wantErr: "nbd export has no nbd details"},
		{name: "nbd kind with block variant", export: VolumeExport{Kind: ExportKindNBD, NBD: nbd, Block: block}, wantErr: "nbd export also sets block details"},
		{name: "empty nbd uri", export: VolumeExport{Kind: ExportKindNBD, NBD: &NBDExport{}}, wantErr: "nbd export has an empty NBD URI"},
		{name: "block kind without block variant", export: VolumeExport{Kind: ExportKindBlock}, wantErr: "block export has no block details"},
		{name: "block kind with nbd variant", export: VolumeExport{Kind: ExportKindBlock, NBD: nbd, Block: block}, wantErr: "block export also sets nbd details"},
		{name: "relative device path", export: VolumeExport{Kind: ExportKindBlock, Block: &BlockExport{DevicePath: "dev/sdb", SizeBytes: 1}}, wantErr: `block export device path "dev/sdb" is not absolute`},
		{name: "empty device path", export: VolumeExport{Kind: ExportKindBlock, Block: &BlockExport{SizeBytes: 1}}, wantErr: `block export device path "" is not absolute`},
		{name: "comma in device path", export: VolumeExport{Kind: ExportKindBlock, Block: &BlockExport{DevicePath: "/dev/disk,unsafe", SizeBytes: 1}}, wantErr: `block export device path "/dev/disk,unsafe" contains a comma`},
		{name: "zero size", export: VolumeExport{Kind: ExportKindBlock, Block: &BlockExport{DevicePath: "/dev/sdb"}}, wantErr: "block export has a zero size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.export.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestPublishedVolumeV1Conversion(t *testing.T) {
	t.Run("NBD is exactly representable", func(t *testing.T) {
		published := &PublishedVolume{VolumeID: "vol-1", NodeID: "node-1", Export: NewNBDExport("nbd+unix:///?socket=/run/vol-1.sock")}
		legacy, err := published.AsV1()
		require.NoError(t, err)
		assert.Equal(t, &PublishedVolumeV1{VolumeID: "vol-1", NodeID: "node-1", NBDURI: "nbd+unix:///?socket=/run/vol-1.sock"}, legacy)
		assert.Equal(t, *published, *PublishedVolumeFromV1(legacy))
	})

	t.Run("block has no safe legacy projection", func(t *testing.T) {
		published := &PublishedVolume{VolumeID: "vol-1", NodeID: "node-1", Export: VolumeExport{Kind: ExportKindBlock, Block: &BlockExport{DevicePath: "/dev/sdb", SizeBytes: 1}}}
		_, err := published.AsV1()
		require.EqualError(t, err, "block export requires schema version 2")
	})
}

// TestPublishedVolumeWireShape pins the JSON the contract carries: a renamed
// tag would decode as an empty variant on a peer at the same schema version.
func TestPublishedVolumeWireShape(t *testing.T) {
	wires := map[string]string{
		"nbd":   `{"volume_id":"vol-1","node_id":"node-1","export":{"kind":"nbd","nbd":{"nbd_uri":"nbd://10.0.0.1:10809"}}}`,
		"block": `{"volume_id":"vol-1","node_id":"node-1","export":{"kind":"block","block":{"device_path":"/dev/sdb","size_bytes":10737418240}}}`,
	}
	for name, wire := range wires {
		t.Run(name, func(t *testing.T) {
			var published PublishedVolume
			require.NoError(t, json.Unmarshal([]byte(wire), &published))
			require.NoError(t, published.Export.Validate())
			encoded, err := json.Marshal(published)
			require.NoError(t, err)
			assert.JSONEq(t, wire, string(encoded))
		})
	}
}
