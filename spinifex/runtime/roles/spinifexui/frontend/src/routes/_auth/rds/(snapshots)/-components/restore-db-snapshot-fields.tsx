import type { Image, SecurityGroup } from "@aws-sdk/client-ec2"
import type { DBSnapshot } from "@aws-sdk/client-rds"
import type { ReactElement } from "react"
import type { DeepPartialSkipArrayKey } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { PageHeading } from "@/components/page-heading"
import { SecurityGroupPicker } from "@/components/security-group-picker"
import { SystemImageRequired } from "@/components/system-image-required"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { isRdsSystemImage, rdsImportCommand } from "@/lib/system-managed"
import {
  type RestoreDBInstanceFormData,
  canRestoreSnapshot,
  suggestedIdentifier,
} from "@/types/rds"

export function restoreDefaults(
  snapshot: DBSnapshot | undefined,
): RestoreDBInstanceFormData {
  const snapshotStorage = snapshot?.AllocatedStorage ?? 0

  return {
    snapshotAllocatedStorage: snapshotStorage,
    dbInstanceIdentifier: suggestedIdentifier(
      snapshot?.DBInstanceIdentifier ?? "",
      "restored",
    ),
    dbInstanceClass: "",
    allocatedStorage: snapshotStorage,
    port: snapshot?.Port ? String(snapshot.Port) : "",
    dbSubnetGroupName: "",
    vpcSecurityGroupIds: [],
    dbParameterGroupName: "",
    deletionProtection: false,
    tags: [],
  }
}

// Everything the CLI panel renders, flattened out of the form state so the
// builder needs no optional handling of its own.
export type RestoreCliValues = Omit<
  RestoreDBInstanceFormData,
  "snapshotAllocatedStorage" | "tags"
>

export function restoreCliValues(
  values: DeepPartialSkipArrayKey<RestoreDBInstanceFormData>,
  snapshotStorage: number,
): RestoreCliValues {
  return {
    dbInstanceIdentifier: values.dbInstanceIdentifier ?? "",
    dbInstanceClass: values.dbInstanceClass ?? "",
    allocatedStorage: values.allocatedStorage ?? snapshotStorage,
    port: values.port ?? "",
    dbSubnetGroupName: values.dbSubnetGroupName ?? "",
    vpcSecurityGroupIds: values.vpcSecurityGroupIds ?? [],
    dbParameterGroupName: values.dbParameterGroupName ?? "",
    deletionProtection: values.deletionProtection ?? false,
  }
}

// The page shown instead of the form when the snapshot cannot be restored, or
// undefined when it can.
export function restoreBlockedView(
  snapshot: DBSnapshot | undefined,
  images: Image[],
  dbSnapshotIdentifier: string,
): ReactElement | undefined {
  if (!snapshot?.DBSnapshotIdentifier) {
    return (
      <>
        <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>
        <p className="text-muted-foreground">DB snapshot not found.</p>
      </>
    )
  }

  // A snapshot still being taken has no data to restore from, and the backend
  // refuses it rather than waiting.
  if (!canRestoreSnapshot(snapshot.Status)) {
    return (
      <>
        <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>
        <PageHeading subtitle={dbSnapshotIdentifier} title="Restore Snapshot" />
        <p className="text-muted-foreground">
          {dbSnapshotIdentifier} is {snapshot.Status}. A snapshot can only be
          restored once it is available.
        </p>
      </>
    )
  }

  const engine = snapshot.Engine ?? ""

  // The engine is the snapshot's and cannot be changed, so a cluster without
  // that engine's image cannot run this restore at all.
  if (
    !images.some((image) =>
      isRdsSystemImage(image, engine, snapshot.EngineVersion ?? ""),
    )
  ) {
    return (
      <>
        <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>
        <PageHeading subtitle={dbSnapshotIdentifier} title="Restore Snapshot" />
        <SystemImageRequired
          description={`This snapshot holds ${engine} data and can only be restored onto a ${engine} instance, but no ${engine} image is imported on this cluster.`}
          importCommand={rdsImportCommand(engine)}
          title={`${engine} image not found`}
        />
      </>
    )
  }

  return undefined
}

export function RestoringFromSection({
  dbSnapshotIdentifier,
  snapshot,
}: {
  dbSnapshotIdentifier: string
  snapshot: DBSnapshot
}) {
  const snapshotStorage = snapshot.AllocatedStorage ?? 0

  return (
    <PanelSection title="Restoring from">
      <PanelFields>
        <PanelField label="Snapshot" mono>
          {dbSnapshotIdentifier}
        </PanelField>
        <PanelField label="Source DB instance" mono>
          {snapshot.DBInstanceIdentifier}
        </PanelField>
        <PanelField label="Engine">
          {[snapshot.Engine ?? "", snapshot.EngineVersion]
            .filter(Boolean)
            .join(" ") || undefined}
        </PanelField>
        <PanelField label="Master username">
          {snapshot.MasterUsername}
        </PanelField>
        <PanelField label="Snapshot storage">
          {snapshotStorage ? `${snapshotStorage} GiB` : undefined}
        </PanelField>
      </PanelFields>
    </PanelSection>
  )
}

export function RestoreSecurityGroupsField({
  error,
  groups,
  onChange,
  placementVpcId,
  selected,
}: {
  error: { message?: string } | undefined
  groups: SecurityGroup[]
  onChange: (next: string[]) => void
  placementVpcId: string | undefined
  selected: string[]
}) {
  return (
    <Field>
      <FieldTitle>VPC security groups</FieldTitle>
      <SecurityGroupPicker
        emptyText={
          placementVpcId
            ? `No security groups available in ${placementVpcId}.`
            : "The snapshot does not identify a placement VPC."
        }
        groups={groups}
        onChange={onChange}
        selected={selected}
      />
      <FieldDescription>
        Leave every box clear to inherit the groups the snapshot recorded.
      </FieldDescription>
      <FieldError errors={[error]} />
    </Field>
  )
}
