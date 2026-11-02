import type { Image } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { formatDateTime } from "@/lib/utils"
import { callerIdentityQueryOptions } from "@/queries/sts"

interface AmiDetailsProps {
  image: Image
  showExtendedDetails?: boolean
}

function yesNo(value: boolean | undefined): string {
  return value ? "Yes" : "No"
}

export function AmiDetails({
  image,
  showExtendedDetails = false,
}: AmiDetailsProps) {
  const { data: identity } = useQuery(callerIdentityQueryOptions)

  // Another account's snapshot, such as a system image's, cannot be described
  // by this one, so its ID would only lead to a not-found page.
  const ownsImage =
    identity?.Account !== undefined && identity.Account === image.OwnerId

  return (
    <>
      <PanelSection
        title={showExtendedDetails ? "AMI information" : "AMI details"}
      >
        <PanelFields>
          <PanelField copy={image.ImageId} label="AMI ID" mono>
            {image.ImageId && !showExtendedDetails ? (
              <Link
                className="text-primary hover:underline"
                params={{ id: image.ImageId }}
                to="/ec2/describe-images/$id"
              >
                {image.ImageId}
              </Link>
            ) : (
              image.ImageId
            )}
          </PanelField>
          <PanelField label="AMI name">{image.Name}</PanelField>
          <PanelField label="Description">{image.Description}</PanelField>
          <PanelField label="Architecture">{image.Architecture}</PanelField>
          <PanelField label="Virtualization type">
            {image.VirtualizationType}
          </PanelField>
          <PanelField label="Boot mode">{image.BootMode}</PanelField>
          <PanelField label="Platform">{image.Platform ?? "Linux"}</PanelField>
          <PanelField label="Platform details">
            {image.PlatformDetails}
          </PanelField>
          <PanelField label="Image type">{image.ImageType}</PanelField>
          <PanelField label="Root device type">
            {image.RootDeviceType}
          </PanelField>
          <PanelField label="Root device name" mono>
            {image.RootDeviceName}
          </PanelField>
          <PanelField label="Owner" mono>
            {image.OwnerId}
          </PanelField>
          <PanelField label="State">{image.State}</PanelField>
          {showExtendedDetails && (
            <>
              <PanelField label="Creation date">
                {formatDateTime(image.CreationDate)}
              </PanelField>
              <PanelField label="Public">{yesNo(image.Public)}</PanelField>
            </>
          )}
        </PanelFields>
      </PanelSection>

      {showExtendedDetails &&
        image.BlockDeviceMappings?.map((mapping) => (
          <PanelSection
            key={mapping.DeviceName}
            title={`Block device: ${mapping.DeviceName ?? ""}`}
          >
            <PanelFields>
              <PanelField label="Device name" mono>
                {mapping.DeviceName}
              </PanelField>
              {mapping.Ebs && (
                <>
                  <PanelField label="Volume size">
                    {mapping.Ebs.VolumeSize
                      ? `${mapping.Ebs.VolumeSize} GB`
                      : undefined}
                  </PanelField>
                  <PanelField label="Volume type">
                    {mapping.Ebs.VolumeType}
                  </PanelField>
                  <PanelField label="Delete on termination">
                    {yesNo(mapping.Ebs.DeleteOnTermination)}
                  </PanelField>
                  <PanelField label="Encrypted">
                    {yesNo(mapping.Ebs.Encrypted)}
                  </PanelField>
                  {ownsImage && mapping.Ebs.SnapshotId && (
                    <PanelField
                      copy={mapping.Ebs.SnapshotId}
                      label="Snapshot ID"
                      mono
                    >
                      <Link
                        className="text-primary hover:underline"
                        params={{ id: mapping.Ebs.SnapshotId }}
                        to="/ec2/describe-snapshots/$id"
                      >
                        {mapping.Ebs.SnapshotId}
                      </Link>
                    </PanelField>
                  )}
                </>
              )}
              {mapping.VirtualName && (
                <PanelField label="Virtual name">
                  {mapping.VirtualName}
                </PanelField>
              )}
            </PanelFields>
          </PanelSection>
        ))}
    </>
  )
}
