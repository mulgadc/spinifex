import { zodResolver } from "@hookform/resolvers/zod"
import {
  useQuery,
  useQueryClient,
  useSuspenseQuery,
} from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { useState } from "react"
import { useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  type CliCommand,
} from "@/components/cli-command-panel"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { SystemImageRequired } from "@/components/system-image-required"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { isRdsSystemImage, rdsImportCommand } from "@/lib/system-managed"
import { useRestoreDBInstanceFromDBSnapshot } from "@/mutations/rds"
import {
  ec2ImagesQueryOptions,
  ec2SecurityGroupsQueryOptions,
} from "@/queries/ec2"
import {
  rdsDBSnapshotQueryOptions,
  rdsEngineVersionsQueryOptions,
  rdsOrderableOptionsQueryOptions,
  rdsParameterGroupsQueryOptions,
  rdsSubnetGroupsQueryOptions,
} from "@/queries/rds"
import {
  type RestoreDBInstanceFormData,
  canRestoreSnapshot,
  restoreDBInstanceSchema,
  suggestedIdentifier,
} from "@/types/rds"

import { pickerNotice } from "../../-components/picker-notice"
import {
  DeletionProtectionField,
  parameterGroupsForEngine,
  RdsSelectField,
} from "../../-components/rds-form-fields"
import {
  SecurityGroupCheckboxes,
  defaultSecurityGroupIdForVpc,
  securityGroupIdsForVpc,
  securityGroupsForVpc,
} from "../../-components/security-group-checkboxes"
import { TagsFieldArray } from "../../-components/tags-field-array"

interface Props {
  dbSnapshotIdentifier: string
}

// What the restore takes from the snapshot rather than from the form, because
// the new instance starts on the snapshot's datadir.
const INHERITED_FIELDS: { label: string; note: string }[] = [
  {
    label: "Engine and version",
    note: "The datadir is written in one engine's on-disk format; restoring it as another is refused.",
  },
  {
    label: "Master username and password",
    note: "The datadir already holds the master role and its password hash, so no bootstrap runs.",
  },
  {
    label: "Initial database",
    note: "A restore cannot create a database the snapshot does not hold.",
  },
  {
    label: "Backup retention and windows",
    note: "The restored instance takes the platform defaults; change them with a modify afterwards.",
  },
]

export function RestoreDBSnapshotPage({ dbSnapshotIdentifier }: Props) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const restoreInstance = useRestoreDBInstanceFromDBSnapshot()
  const [isRechecking, setIsRechecking] = useState(false)

  const { data: snapshotData } = useSuspenseQuery(
    rdsDBSnapshotQueryOptions(dbSnapshotIdentifier),
  )
  const { data: engineVersionsData } = useSuspenseQuery(
    rdsEngineVersionsQueryOptions,
  )
  const { data: subnetGroupsData } = useSuspenseQuery(
    rdsSubnetGroupsQueryOptions,
  )
  const { data: parameterGroupsData } = useSuspenseQuery(
    rdsParameterGroupsQueryOptions,
  )
  const { data: securityGroupsData } = useSuspenseQuery(
    ec2SecurityGroupsQueryOptions,
  )
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)

  const snapshot = snapshotData.DBSnapshots?.[0]
  const engine = snapshot?.Engine ?? ""
  const snapshotStorage = snapshot?.AllocatedStorage ?? 0

  const orderableQuery = useQuery(rdsOrderableOptionsQueryOptions(engine))

  const {
    clearErrors,
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setError,
    setValue,
  } = useForm<RestoreDBInstanceFormData>({
    resolver: zodResolver(restoreDBInstanceSchema),
    defaultValues: {
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
    },
  })

  const values = useWatch({ control })
  const selectedSubnetGroup = values.dbSubnetGroupName ?? ""
  const selectedSecurityGroups = values.vpcSecurityGroupIds ?? []

  const handleRecheck = async () => {
    setIsRechecking(true)
    try {
      await queryClient.invalidateQueries({
        queryKey: ec2ImagesQueryOptions.queryKey,
      })
    } finally {
      setIsRechecking(false)
    }
  }

  const setSecurityGroups = (next: string[]) => {
    clearErrors("vpcSecurityGroupIds")
    setValue("vpcSecurityGroupIds", next, { shouldValidate: true })
  }

  const handleSubnetGroupChange = (name: string | null) => {
    // oxlint-disable-next-line typescript/prefer-nullish-coalescing -- Select reports an explicit null when cleared
    const nextName = name === null ? "" : name
    const nextVpcId =
      subnetGroups.find((group) => group.DBSubnetGroupName === nextName)
        ?.VpcId ?? snapshot?.VpcId
    let nextSecurityGroups = securityGroupIdsForVpc(
      allSecurityGroups,
      nextVpcId,
      selectedSecurityGroups,
    )
    if (
      nextName !== "" &&
      nextVpcId !== snapshot?.VpcId &&
      nextSecurityGroups.length === 0
    ) {
      const defaultGroupId = defaultSecurityGroupIdForVpc(
        allSecurityGroups,
        nextVpcId,
      )
      nextSecurityGroups = defaultGroupId ? [defaultGroupId] : []
    }
    setValue("dbSubnetGroupName", nextName, { shouldValidate: true })
    setSecurityGroups(nextSecurityGroups)
  }

  const onSubmit = async (data: RestoreDBInstanceFormData) => {
    if (
      data.dbSubnetGroupName !== "" &&
      placementVpcId !== snapshot?.VpcId &&
      data.vpcSecurityGroupIds.length === 0
    ) {
      setError("vpcSecurityGroupIds", {
        message: "Select a security group in the new placement VPC",
      })
      return
    }
    try {
      await restoreInstance.mutateAsync({ ...data, dbSnapshotIdentifier })
    } catch {
      // The banner above carries the refusal; the form stays as it was typed.
      return
    }
    await navigate({
      to: "/rds/describe-db-instances/$id",
      params: { id: data.dbInstanceIdentifier },
    })
  }

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

  const images = imagesData.Images ?? []
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
          isRechecking={isRechecking}
          onRecheck={handleRecheck}
          title={`${engine} image not found`}
        />
      </>
    )
  }

  const orderableOptions = orderableQuery.data?.OrderableDBInstanceOptions ?? []
  const instanceClasses = [
    ...new Set(
      orderableOptions.map((o) => o.DBInstanceClass ?? "").filter(Boolean),
    ),
  ]
  const classNotice = pickerNotice(
    orderableQuery,
    instanceClasses.length === 0,
    {
      loading: "Loading the instance classes…",
      failed: "Could not read the instance classes",
      empty: `No instance class this cluster's nodes can run is available for ${engine}.`,
    },
  )

  const subnetGroups = subnetGroupsData.DBSubnetGroups ?? []
  const parameterGroups = parameterGroupsForEngine(
    parameterGroupsData.DBParameterGroups ?? [],
    engineVersionsData.DBEngineVersions ?? [],
    engine,
  )

  const subnetGroupVpc = subnetGroups.find(
    (group) => group.DBSubnetGroupName === selectedSubnetGroup,
  )?.VpcId
  const placementVpcId = subnetGroupVpc ?? snapshot.VpcId
  const allSecurityGroups = securityGroupsData.SecurityGroups ?? []
  const securityGroups = securityGroupsForVpc(allSecurityGroups, placementVpcId)

  return (
    <>
      <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>
      <PageHeading subtitle={dbSnapshotIdentifier} title="Restore Snapshot" />

      {restoreInstance.error && (
        <ErrorBanner
          error={restoreInstance.error}
          msg="Failed to restore the DB snapshot"
        />
      )}

      <div className="max-w-4xl space-y-6">
        <DetailCard>
          <DetailCard.Header>Restoring From</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Snapshot" value={dbSnapshotIdentifier} />
            <DetailRow
              label="Source DB instance"
              value={snapshot.DBInstanceIdentifier}
            />
            <DetailRow
              label="Engine"
              value={[engine, snapshot.EngineVersion].filter(Boolean).join(" ")}
            />
            <DetailRow
              label="Master username"
              value={snapshot.MasterUsername}
            />
            <DetailRow
              label="Snapshot storage"
              value={snapshotStorage ? `${snapshotStorage} GiB` : undefined}
            />
          </DetailCard.Content>
        </DetailCard>

        <form className="space-y-6" onSubmit={handleSubmit(onSubmit)}>
          <Field>
            <FieldTitle>
              <label htmlFor="restore-identifier">
                New DB instance identifier
              </label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.dbInstanceIdentifier}
              id="restore-identifier"
              {...register("dbInstanceIdentifier")}
            />
            <FieldDescription>
              A restore always creates a new instance; it never writes back over
              the one the snapshot came from.
            </FieldDescription>
            <FieldError errors={[errors.dbInstanceIdentifier]} />
          </Field>

          <RdsSelectField
            control={control}
            id="restore-class"
            label="DB instance class"
            name="dbInstanceClass"
            notice={classNotice}
            options={instanceClasses.map((c) => ({ value: c, label: c }))}
            placeholder="Select an instance class"
          />

          <Field>
            <FieldTitle>
              <label htmlFor="restore-storage">Allocated storage (GiB)</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.allocatedStorage}
              id="restore-storage"
              min={snapshotStorage}
              type="number"
              {...register("allocatedStorage", { valueAsNumber: true })}
            />
            <FieldDescription>
              The snapshot holds {snapshotStorage} GiB. A restore may grow the
              volume but never shrink it.
            </FieldDescription>
            <FieldError errors={[errors.allocatedStorage]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="restore-port">Port</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.port}
              id="restore-port"
              placeholder="Snapshot's port"
              {...register("port")}
            />
            <FieldDescription>
              Leave blank to keep the port the snapshot was taken on. The port
              is fixed once the instance exists.
            </FieldDescription>
            <FieldError errors={[errors.port]} />
          </Field>

          <RdsSelectField
            control={control}
            description="Leave unset to place the restored endpoint in the group the snapshot recorded."
            id="restore-subnet-group"
            label="DB subnet group"
            name="dbSubnetGroupName"
            onValueChange={handleSubnetGroupChange}
            options={subnetGroups.map((g) => ({
              value: g.DBSubnetGroupName ?? "",
              label: `${g.DBSubnetGroupName} (${g.VpcId})`,
            }))}
            placeholder="The snapshot's subnet group"
          />

          <Field>
            <FieldTitle>VPC security groups</FieldTitle>
            <SecurityGroupCheckboxes
              emptyText={
                placementVpcId
                  ? `No security groups available in ${placementVpcId}.`
                  : "The snapshot does not identify a placement VPC."
              }
              groups={securityGroups}
              onChange={setSecurityGroups}
              selected={selectedSecurityGroups}
            />
            <FieldDescription>
              Leave every box clear to inherit the groups the snapshot recorded.
            </FieldDescription>
            <FieldError errors={[errors.vpcSecurityGroupIds]} />
          </Field>

          <RdsSelectField
            control={control}
            id="restore-parameter-group"
            label="DB parameter group"
            name="dbParameterGroupName"
            options={parameterGroups.map((g) => ({
              value: g.DBParameterGroupName ?? "",
              label: g.DBParameterGroupName ?? "",
            }))}
            placeholder="The snapshot's parameter group"
          />

          <DeletionProtectionField
            control={control}
            name="deletionProtection"
          />

          <TagsFieldArray control={control} name="tags" />

          <div className="space-y-2 rounded-md border border-border p-4">
            <h3 className="text-xs font-medium">Inherited from the snapshot</h3>
            {INHERITED_FIELDS.map((entry) => (
              <p className="text-xs text-muted-foreground" key={entry.label}>
                <span className="font-medium">{entry.label}</span> —{" "}
                {entry.note}
              </p>
            ))}
          </div>

          <CliCommandPanel
            commands={buildRestoreCommands(dbSnapshotIdentifier, {
              dbInstanceIdentifier: values.dbInstanceIdentifier ?? "",
              dbInstanceClass: values.dbInstanceClass ?? "",
              allocatedStorage: values.allocatedStorage ?? snapshotStorage,
              port: values.port ?? "",
              dbSubnetGroupName: selectedSubnetGroup,
              vpcSecurityGroupIds: selectedSecurityGroups,
              dbParameterGroupName: values.dbParameterGroupName ?? "",
              deletionProtection: values.deletionProtection ?? false,
            })}
          />

          <FormActions
            isPending={restoreInstance.isPending}
            isSubmitting={isSubmitting}
            onCancel={async () =>
              await navigate({ to: "/rds/describe-db-snapshots" })
            }
            pendingLabel="Restoring…"
            submitLabel="Restore Snapshot"
          />
        </form>
      </div>
    </>
  )
}

// Everything the CLI panel renders, flattened out of the form state so the
// builder needs no optional handling of its own.
type CliValues = Omit<
  RestoreDBInstanceFormData,
  "snapshotAllocatedStorage" | "tags"
>

function buildRestoreCommands(
  dbSnapshotIdentifier: string,
  values: CliValues,
): CliCommand[] {
  const parts: CliCommand["parts"] = [
    {
      type: "bin",
      value:
        "AWS_PROFILE=spinifex aws rds restore-db-instance-from-db-snapshot",
    },
    ...commandFlag("--db-snapshot-identifier", dbSnapshotIdentifier),
    ...commandFlag(
      "--db-instance-identifier",
      cliPlaceholder(values.dbInstanceIdentifier, "DBInstanceIdentifier"),
    ),
    ...commandFlag(
      "--db-instance-class",
      cliPlaceholder(values.dbInstanceClass, "DBInstanceClass"),
    ),
    ...commandFlag("--allocated-storage", values.allocatedStorage),
    ...optionalFlag("--port", values.port),
    ...optionalFlag("--db-subnet-group-name", values.dbSubnetGroupName),
    ...optionalFlag(
      "--vpc-security-group-ids",
      values.vpcSecurityGroupIds.join(" "),
    ),
    ...optionalFlag("--db-parameter-group-name", values.dbParameterGroupName),
    ...optionalFlag("--deletion-protection", values.deletionProtection),
  ]

  return [{ label: "Restore DB Instance", parts }]
}
