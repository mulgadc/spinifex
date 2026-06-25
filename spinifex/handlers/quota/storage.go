package handlers_quota

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_volume "github.com/mulgadc/spinifex/spinifex/gateway/ec2/volume"
	"github.com/nats-io/nats.go"
)

// EnforceVolumeCreate gates CreateVolume on the account's live storage usage:
// the sum of its existing volume sizes plus requestedGiB must stay within the
// VolumesGiB cap. Storage is live-counted, so a delete frees quota on the next
// describe. Root/AMI volumes are created out-of-band by the launch path and
// never appear in DescribeVolumes, so they are neither summed nor charged.
func (s *Service) EnforceVolumeCreate(natsConn *nats.Conn, accountID string, requestedGiB int) error {
	if s.Exempt(accountID) {
		return nil
	}
	total, _, err := s.volumeUsage(natsConn, accountID, "")
	if err != nil {
		return err
	}
	return s.EnforceLive(ResourceStorage, total, requestedGiB)
}

// EnforceVolumeModify gates ModifyVolume on the account's live storage usage.
// The resized volume's current size is already in the live sum, so the growth
// charged is newGiB - oldGiB; AWS only grows volumes, so this delta is >= 0 and
// a shrink can never falsely trip the cap.
func (s *Service) EnforceVolumeModify(natsConn *nats.Conn, accountID, volumeID string, newGiB int) error {
	if s.Exempt(accountID) {
		return nil
	}
	total, oldGiB, err := s.volumeUsage(natsConn, accountID, volumeID)
	if err != nil {
		return err
	}
	return s.EnforceLive(ResourceStorage, total-oldGiB, newGiB)
}

// volumeUsage sums the account's volume sizes in GiB via a single DescribeVolumes
// sweep and, when volumeID is non-empty, also returns that volume's current size
// (0 if absent). One pass serves both the create and modify checks.
func (s *Service) volumeUsage(natsConn *nats.Conn, accountID, volumeID string) (total, target int, err error) {
	out, err := gateway_ec2_volume.DescribeVolumes(&ec2.DescribeVolumesInput{}, natsConn, accountID)
	if err != nil {
		return 0, 0, err
	}
	total, target = sumVolumeGiB(out.Volumes, volumeID)
	return total, target, nil
}

// sumVolumeGiB totals the sizes of volumes in GiB and, when volumeID is non-empty,
// returns that volume's size as target. Unsized volumes contribute nothing.
func sumVolumeGiB(volumes []*ec2.Volume, volumeID string) (total, target int) {
	for _, v := range volumes {
		if v == nil || v.Size == nil {
			continue
		}
		size := int(*v.Size)
		total += size
		if volumeID != "" && v.VolumeId != nil && *v.VolumeId == volumeID {
			target = size
		}
	}
	return total, target
}
