import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  type CliCommand,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { SelectField, TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { PageHeading } from "@/components/page-heading"
import {
  defaultSecurityGroupIdForVpc,
  securityGroupIdsForVpc,
  securityGroupsForVpc,
} from "@/components/security-group-picker"
import { TagsFieldArray } from "@/components/tags-field-array"
import { useRestoreDBInstanceFromDBSnapshot } from "@/mutations/rds"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
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
  restoreDBInstanceSchema,
} from "@/types/rds"

import {
  DeletionProtectionField,
  FieldNotes,
  instanceClassOptions,
  parameterGroupOptions,
  parameterGroupsForEngine,
  subnetGroupOptions,
} from "../../-components/rds-form-fields"
import {
  type RestoreCliValues,
  restoreBlockedView,
  restoreCliValues,
  restoreDefaults,
  RestoreSecurityGroupsField,
  RestoringFromSection,
} from "./restore-db-snapshot-fields"

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
  const restoreInstance = useRestoreDBInstanceFromDBSnapshot()

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
    defaultValues: restoreDefaults(snapshot),
  })

  const values = useWatch({ control })
  const selectedSubnetGroup = values.dbSubnetGroupName ?? ""
  const selectedSecurityGroups = values.vpcSecurityGroupIds ?? []

  const setSecurityGroups = (next: string[]) => {
    clearErrors("vpcSecurityGroupIds")
    setValue("vpcSecurityGroupIds", next, { shouldValidate: true })
  }

  const handleSubnetGroupChange = (name: string) => {
    const nextVpcId =
      subnetGroups.find((group) => group.DBSubnetGroupName === name)?.VpcId ??
      snapshot?.VpcId

    let nextSecurityGroups = securityGroupIdsForVpc(
      allSecurityGroups,
      nextVpcId,
      selectedSecurityGroups,
    )

    if (
      name !== "" &&
      nextVpcId !== snapshot?.VpcId &&
      nextSecurityGroups.length === 0
    ) {
      const defaultGroupId = defaultSecurityGroupIdForVpc(
        allSecurityGroups,
        nextVpcId,
      )

      nextSecurityGroups = defaultGroupId ? [defaultGroupId] : []
    }

    setValue("dbSubnetGroupName", name, { shouldValidate: true })
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

  const blocked = restoreBlockedView(
    snapshot,
    imagesData.Images ?? [],
    dbSnapshotIdentifier,
  )

  const instanceTypes = useQuery(ec2InstanceTypesQueryOptions).data
    ?.InstanceTypes

  if (blocked !== undefined || !snapshot) {
    return blocked
  }

  const { classOptions, classSize, classNotice } = instanceClassOptions(
    orderableQuery,
    engine,
    instanceTypes,
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

      <div className="max-w-4xl space-y-6">
        <RestoringFromSection
          dbSnapshotIdentifier={dbSnapshotIdentifier}
          snapshot={snapshot}
        />

        <form onSubmit={handleSubmit(onSubmit)}>
          <FormLock className="space-y-6" locked={restoreInstance.isPending}>
            <TextField
              description="A restore always creates a new instance; it never writes back over the one the snapshot came from."
              error={errors.dbInstanceIdentifier}
              id="restore-identifier"
              label="New DB instance identifier"
              {...register("dbInstanceIdentifier")}
            />

            <SelectField
              control={control}
              id="restore-class"
              label="DB instance class"
              name="dbInstanceClass"
              notice={classNotice}
              options={classOptions}
              description={classSize(values.dbInstanceClass)}
              placeholder="Select an instance class"
            />

            <TextField
              description={
                <>
                  The snapshot holds {snapshotStorage} GiB. A restore may grow
                  the volume but never shrink it.
                </>
              }
              error={errors.allocatedStorage}
              id="restore-storage"
              label="Allocated storage (GiB)"
              min={snapshotStorage}
              type="number"
              {...register("allocatedStorage", { valueAsNumber: true })}
            />

            <TextField
              description="Leave blank to keep the port the snapshot was taken on. The port is fixed once the instance exists."
              error={errors.port}
              id="restore-port"
              label="Port"
              placeholder="Snapshot's port"
              {...register("port")}
            />

            <SelectField
              control={control}
              description="Leave unset to place the restored endpoint in the group the snapshot recorded."
              id="restore-subnet-group"
              label="DB subnet group"
              name="dbSubnetGroupName"
              onValueChange={handleSubnetGroupChange}
              options={subnetGroupOptions(subnetGroups)}
              placeholder="The snapshot's subnet group"
            />

            <RestoreSecurityGroupsField
              error={errors.vpcSecurityGroupIds}
              groups={securityGroups}
              onChange={setSecurityGroups}
              placementVpcId={placementVpcId}
              selected={selectedSecurityGroups}
            />

            <SelectField
              control={control}
              id="restore-parameter-group"
              label="DB parameter group"
              name="dbParameterGroupName"
              options={parameterGroupOptions(parameterGroups)}
              placeholder="The snapshot's parameter group"
            />

            <DeletionProtectionField
              control={control}
              name="deletionProtection"
            />

            <TagsFieldArray control={control} name="tags" />

            <FieldNotes
              className="space-y-2 rounded-md border border-border p-4"
              entries={INHERITED_FIELDS}
              title="Inherited from the snapshot"
            />

            <CliCommandPanel
              commands={buildRestoreCommands(
                dbSnapshotIdentifier,
                restoreCliValues(values, snapshotStorage),
              )}
            />

            <FormActions
              error={restoreInstance.error}
              errorMsg="Failed to restore the DB snapshot"
              isPending={restoreInstance.isPending}
              isSubmitting={isSubmitting}
              onCancel={async () => {
                await navigate({ to: "/rds/describe-db-snapshots" })
              }}
              pendingLabel="Restoring…"
              submitLabel="Restore Snapshot"
            />
          </FormLock>
        </form>
      </div>
    </>
  )
}

function buildRestoreCommands(
  dbSnapshotIdentifier: string,
  values: RestoreCliValues,
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
