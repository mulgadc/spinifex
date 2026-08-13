package handlers_ec2_instance

import (
	"bytes"
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// varsTemplateBytes stands in for a firmware VARS template. The real one is
// 540,672 bytes, but nothing here depends on the size beyond it being exact.
func varsTemplateBytes() []byte { return bytes.Repeat([]byte{0xAB}, 4096) }

func providerEFIService(t *testing.T, provider ebsprovider.EBSProvider) *InstanceServiceImpl {
	t.Helper()
	return providerRootVolumeService(t, provider, rootVolumeAMILoader(), objectstore.NewMemoryObjectStore())
}

// TestCreateEFIVolume_Provider_SeedsFromTemplate is the core of the EFI
// decouple: the variable store is allocated through the provider with the
// template as seed bytes, so no control-plane engine is built for it.
func TestCreateEFIVolume_Provider_SeedsFromTemplate(t *testing.T) {
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{VolumeSeeding: true})
	svc := providerEFIService(t, provider)
	template := varsTemplateBytes()

	require.NoError(t, svc.createEFIVolumeViaProvider(context.Background(), "vol-efiseed001-efi", template))

	volume, err := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-efiseed001-efi",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(len(template)), volume.CapacityBytes,
		"the store must be sized byte-exactly to the VARS template")

	seed, ok := provider.SeedData("vol-efiseed001-efi")
	require.True(t, ok)
	assert.Equal(t, template, seed, "the template must reach the provider as seed bytes")
}

// A provider that rounds capacity to a GiB boundary produces a VARS region
// pflash refuses, which would surface as an unbootable guest rather than a
// failed launch. The mismatch has to fail the launch instead.
func TestCreateEFIVolume_Provider_CapacityMismatchFails(t *testing.T) {
	provider := &roundingProvider{
		EBSProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{VolumeSeeding: true}),
	}
	svc := providerEFIService(t, provider)

	err := svc.createEFIVolumeViaProvider(context.Background(), "vol-efiround01-efi", varsTemplateBytes())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// A relaunch reissues the same create. Reseeding would overwrite the BootOrder
// the guest wrote on its first boot, so the second call must be a no-op.
func TestCreateEFIVolume_Provider_RepeatDoesNotReseed(t *testing.T) {
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{VolumeSeeding: true})
	svc := providerEFIService(t, provider)
	ctx := context.Background()
	template := varsTemplateBytes()

	require.NoError(t, svc.createEFIVolumeViaProvider(ctx, "vol-efirepeat1-efi", template))
	require.NoError(t, svc.createEFIVolumeViaProvider(ctx, "vol-efirepeat1-efi", template))

	seed, ok := provider.SeedData("vol-efirepeat1-efi")
	require.True(t, ok)
	assert.Equal(t, template, seed)
}

// A seed above the wire bound must fail the launch with an error naming the
// limit, not reach NATS and fail as an oversized publish.
func TestCreateEFIVolume_Provider_OversizedTemplateFails(t *testing.T) {
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{VolumeSeeding: true})
	svc := providerEFIService(t, provider)

	err := svc.createEFIVolumeViaProvider(context.Background(), "vol-efitoobig1-efi", make([]byte, ebsprovider.MaxSeedBytes+1))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// TestPrepareEFIVolume_Provider_RegistersEBSRequest covers the whole function
// against this host's real firmware, including the name the VM launches with.
func TestPrepareEFIVolume_Provider_RegistersEBSRequest(t *testing.T) {
	if _, _, _, err := vm.FirmwarePaths("x86_64"); err != nil {
		t.Skipf("no UEFI firmware on this host: %v", err)
	}

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{VolumeSeeding: true})
	svc := providerEFIService(t, provider)
	instance := &vm.VM{}

	require.NoError(t, svc.prepareEFIVolume(context.Background(), "vol-efiprepare001", instance, "x86_64"))

	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()
	require.Len(t, instance.EBSRequests.Requests, 1)
	assert.Equal(t, "vol-efiprepare001-efi", instance.EBSRequests.Requests[0].Name)
	assert.True(t, instance.EBSRequests.Requests[0].EFI)
	assert.False(t, instance.EBSRequests.Requests[0].Boot)

	seed, ok := provider.SeedData("vol-efiprepare001-efi")
	require.True(t, ok)
	assert.NotEmpty(t, seed, "the store must be seeded from this host's VARS template")
}

// roundingProvider reports a GiB-rounded capacity the way a provider with no
// sub-GiB volume granularity would.
type roundingProvider struct {
	ebsprovider.EBSProvider
}

func (p *roundingProvider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	volume, err := p.EBSProvider.CreateVolume(ctx, req)
	if err != nil {
		return nil, err
	}
	volume.CapacityBytes = 1 << 30
	return volume, nil
}
