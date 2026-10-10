import type { Image } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"

import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { formatDateTime } from "@/lib/utils"

interface AmiDetailsProps {
  image: Image
  showExtendedDetails?: boolean
}

export function AmiDetails({
  image,
  showExtendedDetails = false,
}: AmiDetailsProps) {
  const creationDate = formatDateTime(image.CreationDate)

  return (
    <>
      {/* AMI Details */}
      <DetailCard>
        <DetailCard.Header>
          {showExtendedDetails ? "AMI Information" : "AMI Details"}
        </DetailCard.Header>
        <DetailCard.Content>
          <DetailRow
            label="AMI ID"
            value={
              image.ImageId && !showExtendedDetails ? (
                <Link
                  className="text-primary hover:underline"
                  params={{ id: image.ImageId }}
                  to="/ec2/describe-images/$id"
                >
                  {image.ImageId}
                </Link>
              ) : (
                image.ImageId
              )
            }
          />
          <DetailRow label="AMI Name" value={image.Name} />
          <DetailRow label="Description" value={image.Description} />
          <DetailRow label="Architecture" value={image.Architecture} />
          <DetailRow
            label="Virtualization Type"
            value={image.VirtualizationType}
          />
          <DetailRow label="Platform" value={image.Platform ?? "Linux"} />
          <DetailRow label="Platform Details" value={image.PlatformDetails} />
          <DetailRow label="Image Type" value={image.ImageType} />
          <DetailRow label="Root Device Type" value={image.RootDeviceType} />
          <DetailRow label="Root Device Name" value={image.RootDeviceName} />
          <DetailRow label="Owner" value={image.OwnerId} />
          <DetailRow label="State" value={image.State} />
          {showExtendedDetails && (
            <>
              <DetailRow label="Creation Date" value={creationDate} />
              <DetailRow label="Public" value={image.Public ? "Yes" : "No"} />
            </>
          )}
        </DetailCard.Content>
      </DetailCard>

      {/* Block Device Mappings */}
      {showExtendedDetails &&
        image.BlockDeviceMappings &&
        image.BlockDeviceMappings.length > 0 &&
        image.BlockDeviceMappings.map((mapping) => (
          <DetailCard key={mapping.DeviceName}>
            <DetailCard.Header>
              Block Device: {mapping.DeviceName}
            </DetailCard.Header>
            <DetailCard.Content>
              <DetailRow label="Device Name" value={mapping.DeviceName} />
              {mapping.Ebs && (
                <>
                  <DetailRow
                    label="Volume Size"
                    value={
                      mapping.Ebs.VolumeSize
                        ? `${mapping.Ebs.VolumeSize} GB`
                        : undefined
                    }
                  />
                  <DetailRow
                    label="Volume Type"
                    value={mapping.Ebs.VolumeType}
                  />
                  <DetailRow
                    label="Delete On Termination"
                    value={mapping.Ebs.DeleteOnTermination ? "Yes" : "No"}
                  />
                  <DetailRow
                    label="Encrypted"
                    value={mapping.Ebs.Encrypted ? "Yes" : "No"}
                  />
                  {mapping.Ebs.SnapshotId && (
                    <DetailRow
                      label="Snapshot ID"
                      value={
                        <Link
                          className="text-primary hover:underline"
                          params={{ id: mapping.Ebs.SnapshotId }}
                          to="/ec2/describe-snapshots/$id"
                        >
                          {mapping.Ebs.SnapshotId}
                        </Link>
                      }
                    />
                  )}
                </>
              )}
              {mapping.VirtualName && (
                <DetailRow label="Virtual Name" value={mapping.VirtualName} />
              )}
            </DetailCard.Content>
          </DetailCard>
        ))}
    </>
  )
}
