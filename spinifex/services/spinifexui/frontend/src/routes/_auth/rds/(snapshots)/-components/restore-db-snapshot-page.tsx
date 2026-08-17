import { zodResolver } from "@hookform/resolvers/zod"
import {
  useQuery,
  useQueryClient,
  useSuspenseQuery,
} from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Plus, Trash2 } from "lucide-react"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  cliPlaceholder,
  type CliCommand,
} from "@/components/cli-command-panel"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { SystemImageRequired } from "@/components/system-image-required"
import { Button } from "@/components/ui/button"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { isRdsSystemImage, rdsImportCommand } from "@/lib/system-managed"
import { securityGroupLabel } from "@/lib/utils"
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

import { PickerNoticeText, pickerNotice } from "../../-components/picker-notice"

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
    control,
    formState: { errors, isSubmitting },
    getValues,
    handleSubmit,
    register,
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

  const toggleSecurityGroup = (groupId: string) => {
    const next = selectedSecurityGroups.includes(groupId)
      ? selectedSecurityGroups.filter((id) => id !== groupId)
      : [...selectedSecurityGroups, groupId]
    setValue("vpcSecurityGroupIds", next, { shouldValidate: true })
  }

  const onSubmit = async (data: RestoreDBInstanceFormData) => {
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
        <PageHeading subtitle={dbSnapshotIdentifier} title="Restore snapshot" />
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
  if (!images.some((image) => isRdsSystemImage(image, engine))) {
    return (
      <>
        <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>
        <PageHeading subtitle={dbSnapshotIdentifier} title="Restore snapshot" />
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
  const engineFamilies = new Set(
    (engineVersionsData.DBEngineVersions ?? [])
      .filter((v) => v.Engine === engine)
      .map((v) => v.DBParameterGroupFamily),
  )
  const parameterGroups = (parameterGroupsData.DBParameterGroups ?? []).filter(
    (g) => engineFamilies.has(g.DBParameterGroupFamily),
  )

  const subnetGroupVpc = subnetGroups.find(
    (g) => g.DBSubnetGroupName === selectedSubnetGroup,
  )?.VpcId
  const allSecurityGroups = securityGroupsData.SecurityGroups ?? []
  const securityGroups = subnetGroupVpc
    ? allSecurityGroups.filter((g) => g.VpcId === subnetGroupVpc)
    : allSecurityGroups

  return (
    <>
      <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>
      <PageHeading subtitle={dbSnapshotIdentifier} title="Restore snapshot" />

      {restoreInstance.error && (
        <ErrorBanner
          error={restoreInstance.error}
          msg="Failed to restore the DB snapshot"
        />
      )}

      <div className="max-w-4xl space-y-6">
        <DetailCard>
          <DetailCard.Header>Restoring from</DetailCard.Header>
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

          <Field>
            <FieldTitle>
              <label htmlFor="restore-class">DB instance class</label>
            </FieldTitle>
            {classNotice ? (
              <PickerNoticeText notice={classNotice} />
            ) : (
              <Controller
                control={control}
                name="dbInstanceClass"
                render={({ field }) => (
                  <Select onValueChange={field.onChange} value={field.value}>
                    <SelectTrigger
                      aria-invalid={!!errors.dbInstanceClass}
                      className="w-full"
                      id="restore-class"
                    >
                      <SelectValue placeholder="Select an instance class" />
                    </SelectTrigger>
                    <SelectContent>
                      {instanceClasses.map((instanceClass) => (
                        <SelectItem key={instanceClass} value={instanceClass}>
                          {instanceClass}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
            )}
            <FieldError errors={[errors.dbInstanceClass]} />
          </Field>

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

          <Field>
            <FieldTitle>
              <label htmlFor="restore-subnet-group">DB subnet group</label>
            </FieldTitle>
            <Controller
              control={control}
              name="dbSubnetGroupName"
              render={({ field }) => (
                <Select onValueChange={field.onChange} value={field.value}>
                  <SelectTrigger className="w-full" id="restore-subnet-group">
                    <SelectValue placeholder="The snapshot's subnet group" />
                  </SelectTrigger>
                  <SelectContent>
                    {subnetGroups.map((group) => (
                      <SelectItem
                        key={group.DBSubnetGroupName}
                        value={group.DBSubnetGroupName ?? ""}
                      >
                        {group.DBSubnetGroupName} ({group.VpcId})
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            />
            <FieldDescription>
              Leave unset to place the restored endpoint in the group the
              snapshot recorded.
            </FieldDescription>
            <FieldError errors={[errors.dbSubnetGroupName]} />
          </Field>

          <Field>
            <FieldTitle>VPC security groups</FieldTitle>
            {securityGroups.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                No security groups available
                {subnetGroupVpc ? ` in ${subnetGroupVpc}` : ""}.
              </p>
            ) : (
              <div className="space-y-1">
                {securityGroups.map((group) => (
                  <label
                    className="flex items-center gap-2 text-xs"
                    key={group.GroupId}
                  >
                    <input
                      aria-label={`Security group ${securityGroupLabel(group)}`}
                      checked={selectedSecurityGroups.includes(
                        group.GroupId ?? "",
                      )}
                      onChange={() => toggleSecurityGroup(group.GroupId ?? "")}
                      type="checkbox"
                    />
                    <span className="font-mono">
                      {securityGroupLabel(group)}
                    </span>
                  </label>
                ))}
              </div>
            )}
            <FieldDescription>
              Leave every box clear to inherit the groups the snapshot recorded.
            </FieldDescription>
            <FieldError errors={[errors.vpcSecurityGroupIds]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="restore-parameter-group">
                DB parameter group
              </label>
            </FieldTitle>
            <Controller
              control={control}
              name="dbParameterGroupName"
              render={({ field }) => (
                <Select onValueChange={field.onChange} value={field.value}>
                  <SelectTrigger
                    className="w-full"
                    id="restore-parameter-group"
                  >
                    <SelectValue placeholder="The snapshot's parameter group" />
                  </SelectTrigger>
                  <SelectContent>
                    {parameterGroups.map((group) => (
                      <SelectItem
                        key={group.DBParameterGroupName}
                        value={group.DBParameterGroupName ?? ""}
                      >
                        {group.DBParameterGroupName}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            />
            <FieldError errors={[errors.dbParameterGroupName]} />
          </Field>

          <Field>
            <FieldTitle>Deletion protection</FieldTitle>
            <Controller
              control={control}
              name="deletionProtection"
              render={({ field }) => (
                <label className="flex items-center gap-2 text-xs">
                  <input
                    aria-label="Enable deletion protection"
                    checked={field.value}
                    onChange={(e) => field.onChange(e.target.checked)}
                    type="checkbox"
                  />
                  <span>Refuse DeleteDBInstance while this is on</span>
                </label>
              )}
            />
          </Field>

          <Field>
            <FieldTitle>Tags</FieldTitle>
            <div className="space-y-2">
              {(values.tags ?? []).map((_, index) => (
                // oxlint-disable-next-line react/no-array-index-key -- form array with no stable id
                <div className="flex items-center gap-2" key={index}>
                  <Input placeholder="Key" {...register(`tags.${index}.key`)} />
                  <Input
                    placeholder="Value"
                    {...register(`tags.${index}.value`)}
                  />
                  <Button
                    onClick={() =>
                      setValue(
                        "tags",
                        getValues("tags").filter((__, i) => i !== index),
                      )
                    }
                    size="icon"
                    type="button"
                    variant="ghost"
                  >
                    <Trash2 className="size-3.5" />
                  </Button>
                </div>
              ))}
              <Button
                onClick={() =>
                  setValue("tags", [
                    ...getValues("tags"),
                    { key: "", value: "" },
                  ])
                }
                size="sm"
                type="button"
                variant="outline"
              >
                <Plus className="size-3.5" />
                Add tag
              </Button>
            </div>
          </Field>

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
            submitLabel="Restore snapshot"
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
    { type: "flag", value: " --db-snapshot-identifier" },
    { type: "value", value: ` ${dbSnapshotIdentifier}` },
    { type: "flag", value: " --db-instance-identifier" },
    {
      type: "value",
      value: ` ${cliPlaceholder(values.dbInstanceIdentifier, "DBInstanceIdentifier")}`,
    },
    { type: "flag", value: " --db-instance-class" },
    {
      type: "value",
      value: ` ${cliPlaceholder(values.dbInstanceClass, "DBInstanceClass")}`,
    },
    { type: "flag", value: " --allocated-storage" },
    { type: "value", value: ` ${values.allocatedStorage}` },
  ]

  if (values.port) {
    parts.push(
      { type: "flag", value: " --port" },
      { type: "value", value: ` ${values.port}` },
    )
  }
  if (values.dbSubnetGroupName) {
    parts.push(
      { type: "flag", value: " --db-subnet-group-name" },
      { type: "value", value: ` ${values.dbSubnetGroupName}` },
    )
  }
  if (values.vpcSecurityGroupIds.length > 0) {
    parts.push(
      { type: "flag", value: " --vpc-security-group-ids" },
      { type: "value", value: ` ${values.vpcSecurityGroupIds.join(" ")}` },
    )
  }
  if (values.dbParameterGroupName) {
    parts.push(
      { type: "flag", value: " --db-parameter-group-name" },
      { type: "value", value: ` ${values.dbParameterGroupName}` },
    )
  }
  if (values.deletionProtection) {
    parts.push({ type: "flag", value: " --deletion-protection" })
  }

  return [{ label: "Restore DB Instance", parts }]
}
