import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery, useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { type FieldErrors, useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  type CliCommand,
} from "@/components/cli-command-panel"
import { PanelSection } from "@/components/detail-panel"
import { FormActions } from "@/components/form-actions"
import { SelectField, TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import {
  securityGroupIdsForVpc,
  securityGroupsForVpc,
} from "@/components/security-group-picker"
import { SystemImageRequired } from "@/components/system-image-required"
import { TagsFieldArray } from "@/components/tags-field-array"
import { rdsImportCommand } from "@/lib/system-managed"
import { useCreateDBInstance } from "@/mutations/rds"
import {
  ec2ImagesQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import {
  rdsEngineVersionsQueryOptions,
  rdsOrderableOptionsQueryOptions,
  rdsParameterGroupsQueryOptions,
  rdsSubnetGroupsQueryOptions,
} from "@/queries/rds"
import {
  type CreateDBInstanceFormData,
  createDBInstanceSchema,
  MAX_BACKUP_RETENTION_DAYS,
  MIN_ALLOCATED_STORAGE_GIB,
} from "@/types/rds"

import {
  DeletionProtectionField,
  useInstanceClassOptions,
  parameterGroupOptions,
  parameterGroupsForEngine,
  subnetGroupOptions,
} from "../../-components/rds-form-fields"
import {
  BackupWindowField,
  MaintenanceWindowField,
} from "../../-components/window-fields"
import {
  AllocatedStorageField,
  type CreateCliValues,
  createCliValues,
  DEFAULT_BACKUP_RETENTION_DAYS,
  EngineField,
  engineImageCatalog,
  EngineVersionField,
  instanceClassNotice,
  engineName,
  PlacementSecurityGroupsField,
} from "./create-db-instance-fields"

// The fields the closed Advanced settings section holds, so a refused submit
// can open it on the error rather than leave the error out of sight.
const ADVANCED_FIELDS = [
  "dbParameterGroupName",
  "backupRetentionPeriod",
  "preferredBackupWindow",
  "preferredMaintenanceWindow",
] as const satisfies readonly (keyof CreateDBInstanceFormData)[]

export function hasAdvancedError(
  errors: FieldErrors<CreateDBInstanceFormData>,
): boolean {
  return ADVANCED_FIELDS.some((name) => errors[name] !== undefined)
}

interface CreateDBInstancePageProps {
  // Called with the new DB instance's identifier once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateDBInstancePage({ onClose }: CreateDBInstancePageProps) {
  const createInstance = useCreateDBInstance()

  // Bumped on a refused submit with an error inside Advanced settings, which
  // remounts the section open; react-hook-form keeps the values across it.
  const [advancedReveals, setAdvancedReveals] = useState(0)

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

  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)

  const images = imagesData.Images ?? []
  const engineVersions = engineVersionsData.DBEngineVersions ?? []

  const {
    engines,
    bootableEngines,
    engineVersionHasImage,
    firstBootableVersion,
    engineHasImage,
  } = engineImageCatalog(engineVersions, images)

  const initialEngine = bootableEngines[0] ?? ""

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
  } = useForm<CreateDBInstanceFormData>({
    resolver: zodResolver(createDBInstanceSchema),
    defaultValues: {
      dbInstanceIdentifier: "",
      engine: initialEngine,
      engineVersion: firstBootableVersion(initialEngine),
      dbInstanceClass: "",
      allocatedStorage: MIN_ALLOCATED_STORAGE_GIB,
      masterUsername: "",
      masterUserPassword: "",
      confirmPassword: "",
      dbName: "",
      port: "",
      dbSubnetGroupName: "",
      vpcSecurityGroupIds: [],
      dbParameterGroupName: "",
      deletionProtection: false,
      backupRetentionPeriod: DEFAULT_BACKUP_RETENTION_DAYS,
      preferredBackupWindow: "",
      preferredMaintenanceWindow: "",
      tags: [],
    },
  })

  const values = useWatch({ control })
  const selectedEngine = values.engine ?? ""
  const selectedSubnetGroup = values.dbSubnetGroupName ?? ""
  const selectedSecurityGroups = values.vpcSecurityGroupIds ?? []

  const orderableQuery = useQuery(
    rdsOrderableOptionsQueryOptions(selectedEngine),
  )

  const versionsForEngine = engineVersions.filter(
    (v) => v.Engine === selectedEngine,
  )

  const {
    classOptions,
    classSize,
    classNotice: engineClassNotice,
  } = useInstanceClassOptions(orderableQuery, selectedEngine)

  const [storageBounds] = orderableQuery.data?.OrderableDBInstanceOptions ?? []

  const subnetGroups = subnetGroupsData.DBSubnetGroups ?? []

  const groupsForEngine = parameterGroupsForEngine(
    parameterGroupsData.DBParameterGroups ?? [],
    engineVersions,
    selectedEngine,
  )

  const subnetGroupVpc = subnetGroups.find(
    (group) => group.DBSubnetGroupName === selectedSubnetGroup,
  )?.VpcId

  const defaultVpcId = vpcsData.Vpcs?.find((vpc) => vpc.IsDefault)?.VpcId
  const placementVpcId = subnetGroupVpc ?? defaultVpcId
  const allSecurityGroups = securityGroupsData.SecurityGroups ?? []
  const securityGroups = securityGroupsForVpc(allSecurityGroups, placementVpcId)

  const handleEngineChange = (engine: string) => {
    setValue("engine", engine, { shouldValidate: true })
    setValue("engineVersion", firstBootableVersion(engine))
    setValue("dbInstanceClass", "")
    setValue("dbParameterGroupName", "")
  }

  const setSecurityGroups = (next: string[]) => {
    setValue("vpcSecurityGroupIds", next, { shouldValidate: true })
  }

  const handleSubnetGroupChange = (name: string) => {
    const nextVpcId =
      subnetGroups.find((group) => group.DBSubnetGroupName === name)?.VpcId ??
      defaultVpcId

    setValue("dbSubnetGroupName", name, { shouldValidate: true })
    setSecurityGroups(
      securityGroupIdsForVpc(
        allSecurityGroups,
        nextVpcId,
        selectedSecurityGroups,
      ),
    )
  }

  const onSubmit = async (data: CreateDBInstanceFormData) => {
    try {
      await createInstance.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(data.dbInstanceIdentifier)
  }

  const onInvalid = (invalid: FieldErrors<CreateDBInstanceFormData>) => {
    if (hasAdvancedError(invalid)) {
      setAdvancedReveals((count) => count + 1)
    }
  }

  // Both engine images absent is a cluster that cannot run RDS at all, so the
  // form is replaced. One absent is the normal state of a fresh cluster and
  // must leave the other engine's create path intact.
  if (engines.length > 0 && bootableEngines.length === 0) {
    return (
      <SystemImageRequired
        description={`RDS boots a Spinifex-managed engine image that is not shipped with the platform. Import one before creating a database. Engines offered: ${engines.join(", ")}.`}
        importCommand={rdsImportCommand(engines[0] ?? "")}
        title="RDS system image not found"
      />
    )
  }

  // Driven by which engines lack an image rather than by the selection: the
  // picker refuses to select one that lacks it, so a callout waiting on the
  // selection is a callout that never appears.
  const enginesMissingImage = engines.filter((e) => !engineHasImage(e))

  const classNotice = instanceClassNotice(selectedEngine, engineClassNotice)

  return (
    <form onSubmit={handleSubmit(onSubmit, onInvalid)}>
      <FormLock
        className="space-y-6"
        locked={createInstance.isPending || isSubmitting}
      >
        <PanelSection title="Engine">
          <div className="space-y-6">
            <EngineField
              control={control}
              engineHasImage={engineHasImage}
              engines={engines}
              error={errors.engine}
              onEngineChange={handleEngineChange}
            />

            {enginesMissingImage.map((engine) => (
              <SystemImageRequired
                description={`No ${engineName(engine)} image is imported on this cluster, so a ${engineName(engine)} database cannot launch. The other engines are unaffected.`}
                importCommand={rdsImportCommand(engine)}
                key={engine}
                title={`${engineName(engine)} image not found`}
              />
            ))}

            <EngineVersionField
              control={control}
              engineVersionHasImage={engineVersionHasImage}
              error={errors.engineVersion}
              selectedEngine={selectedEngine}
              versions={versionsForEngine}
            />
          </div>
        </PanelSection>

        <PanelSection title="Settings">
          <div className="space-y-6">
            <TextField
              error={errors.dbInstanceIdentifier}
              id="db-identifier"
              info="Lowercase letters, digits and hyphens. This is also the endpoint hostname, so it cannot be changed later."
              label="DB instance identifier"
              placeholder="orders-db"
              {...register("dbInstanceIdentifier")}
            />

            <TextField
              error={errors.masterUsername}
              id="db-master-username"
              label="Master username"
              placeholder="dbadmin"
              {...register("masterUsername")}
            />

            <TextField
              error={errors.masterUserPassword}
              id="db-master-password"
              info={
                <>
                  8–128 printable ASCII characters, excluding &apos;/&apos;,
                  &apos;&quot;&apos;, &apos;@&apos; and spaces.
                </>
              }
              label="Master password"
              type="password"
              {...register("masterUserPassword")}
            />

            <TextField
              error={errors.confirmPassword}
              id="db-confirm-password"
              label="Confirm master password"
              type="password"
              {...register("confirmPassword")}
            />

            <TextField
              error={errors.dbName}
              id="db-name"
              info="Leave blank to create no database. Letters, digits and underscores, starting with a letter."
              label="Initial database name"
              placeholder="Optional"
              {...register("dbName")}
            />

            <DeletionProtectionField
              control={control}
              name="deletionProtection"
            />
          </div>
        </PanelSection>

        <PanelSection title="Instance configuration">
          <div className="space-y-6">
            <SelectField
              control={control}
              id="db-instance-class"
              label="DB instance class"
              name="dbInstanceClass"
              notice={classNotice}
              options={classOptions}
              description={classSize(values.dbInstanceClass)}
              placeholder="Select an instance class"
            />

            <AllocatedStorageField
              bounds={storageBounds}
              error={errors.allocatedStorage}
              {...register("allocatedStorage", { valueAsNumber: true })}
            />
          </div>
        </PanelSection>

        <PanelSection title="Connectivity">
          <div className="space-y-6">
            <SelectField
              control={control}
              id="db-subnet-group"
              info="Leave unset to place the endpoint in the account's default VPC subnet. The subnet group cannot be changed after create."
              label="DB subnet group"
              name="dbSubnetGroupName"
              onValueChange={handleSubnetGroupChange}
              options={subnetGroupOptions(subnetGroups)}
              placeholder="Default VPC subnet"
            />

            <PlacementSecurityGroupsField
              error={errors.vpcSecurityGroupIds}
              groups={securityGroups}
              onChange={setSecurityGroups}
              placementVpcId={placementVpcId}
              selected={selectedSecurityGroups}
            />

            <TextField
              error={errors.port}
              id="db-port"
              info="Leave blank to use the engine's own port. The port is fixed at create and cannot be changed later."
              label="Port"
              placeholder="Engine default"
              {...register("port")}
            />
          </div>
        </PanelSection>

        <PanelSection
          defaultOpen={advancedReveals > 0}
          key={advancedReveals}
          title="Advanced settings"
        >
          <div className="space-y-6">
            <SelectField
              control={control}
              id="db-parameter-group"
              label="DB parameter group"
              name="dbParameterGroupName"
              options={parameterGroupOptions(groupsForEngine)}
              placeholder="Engine default group"
            />

            <TextField
              error={errors.backupRetentionPeriod}
              id="db-backup-retention"
              info="0 disables automated backups. There is no point-in-time restore; backups are daily snapshots taken in the backup window."
              label="Backup retention (days)"
              max={MAX_BACKUP_RETENTION_DAYS}
              min={0}
              type="number"
              {...register("backupRetentionPeriod", { valueAsNumber: true })}
            />

            <BackupWindowField
              control={control}
              id="db-backup-window"
              name="preferredBackupWindow"
            />

            <MaintenanceWindowField
              control={control}
              id="db-maintenance-window"
              name="preferredMaintenanceWindow"
            />
          </div>
        </PanelSection>

        <PanelSection title="Tags">
          <TagsFieldArray control={control} name="tags" />
        </PanelSection>

        <CliCommandPanel
          commands={buildCreateDBInstanceCommands(createCliValues(values))}
        />

        <FormActions
          error={createInstance.error}
          errorMsg="Failed to create the DB instance"
          isPending={createInstance.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Database"
        />
      </FormLock>
    </form>
  )
}

function buildCreateDBInstanceCommands(values: CreateCliValues): CliCommand[] {
  const parts: CliCommand["parts"] = [
    { type: "bin", value: "AWS_PROFILE=spinifex aws rds create-db-instance" },
    ...commandFlag(
      "--db-instance-identifier",
      cliPlaceholder(values.dbInstanceIdentifier, "DBInstanceIdentifier"),
    ),
    ...commandFlag("--engine", cliPlaceholder(values.engine, "Engine")),
    ...commandFlag(
      "--db-instance-class",
      cliPlaceholder(values.dbInstanceClass, "DBInstanceClass"),
    ),
    ...commandFlag("--allocated-storage", values.allocatedStorage),
    ...commandFlag(
      "--master-username",
      cliPlaceholder(values.masterUsername, "MasterUsername"),
    ),
    { type: "flag", value: " --master-user-password" },
    { type: "variable", value: " $DB_PASSWORD" },
    ...optionalFlag("--engine-version", values.engineVersion),
    ...optionalFlag("--db-name", values.dbName),
    ...optionalFlag("--port", values.port),
    ...optionalFlag("--db-subnet-group-name", values.dbSubnetGroupName),
    ...optionalFlag(
      "--vpc-security-group-ids",
      values.vpcSecurityGroupIds.join(" "),
    ),
    ...optionalFlag("--db-parameter-group-name", values.dbParameterGroupName),
    ...optionalFlag("--deletion-protection", values.deletionProtection),
    ...commandFlag("--backup-retention-period", values.backupRetentionPeriod),
    ...optionalFlag("--preferred-backup-window", values.preferredBackupWindow),
    ...optionalFlag(
      "--preferred-maintenance-window",
      values.preferredMaintenanceWindow,
    ),
  ]

  return [{ label: "Create DB Instance", parts }]
}
