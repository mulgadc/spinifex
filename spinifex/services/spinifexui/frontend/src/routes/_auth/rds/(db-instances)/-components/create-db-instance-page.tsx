import { zodResolver } from "@hookform/resolvers/zod"
import {
  useQueryClient,
  useSuspenseQuery,
  useQuery,
} from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  type CliCommand,
} from "@/components/cli-command-panel"
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { isRdsSystemImage, rdsImportCommand } from "@/lib/system-managed"
import { useCreateDBInstance } from "@/mutations/rds"
import {
  ec2ImagesQueryOptions,
  ec2SecurityGroupsQueryOptions,
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
  type PickerNotice,
  PickerNoticeText,
  pickerNotice,
} from "../../-components/picker-notice"
import { SecurityGroupCheckboxes } from "../../-components/security-group-checkboxes"
import { TagsFieldArray } from "../../-components/tags-field-array"

// The retention the backend applies when a create names none.
const DEFAULT_BACKUP_RETENTION_DAYS = 7

// Parameters CreateDBInstance rejects outright. Rendered as disabled controls
// rather than omitted: a user arriving from the AWS console looks for these,
// and a visible disabled control answers what a missing one only raises.
const UNAVAILABLE_OPTIONS = [
  {
    id: "multi-az",
    label: "Multi-AZ deployment",
    note: "Not available on Spinifex — the platform is single-AZ, so a standby would not exist.",
  },
  {
    id: "publicly-accessible",
    label: "Public access",
    note: "Not available on Spinifex — the endpoint is a private VPC address.",
  },
  {
    id: "storage-autoscaling",
    label: "Storage autoscaling",
    note: "Not available on Spinifex — allocated storage grows only when you ask it to.",
  },
] as const

export function CreateDBInstancePage() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const createInstance = useCreateDBInstance()
  const [isRechecking, setIsRechecking] = useState(false)

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

  const images = imagesData.Images ?? []
  const engineVersions = engineVersionsData.DBEngineVersions ?? []
  const engines = [
    ...new Set(engineVersions.map((v) => v.Engine ?? "").filter(Boolean)),
  ]
  const engineHasImage = (engine: string) =>
    images.some((image) => isRdsSystemImage(image, engine))
  const bootableEngines = engines.filter(engineHasImage)

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
      engine: bootableEngines[0] ?? "",
      engineVersion: "",
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
  const orderableOptions = orderableQuery.data?.OrderableDBInstanceOptions ?? []
  const instanceClasses = [
    ...new Set(
      orderableOptions.map((o) => o.DBInstanceClass ?? "").filter(Boolean),
    ),
  ]
  const storageBounds = orderableOptions[0]

  const subnetGroups = subnetGroupsData.DBSubnetGroups ?? []
  const parameterGroups = parameterGroupsData.DBParameterGroups ?? []
  // Only the family the selected engine belongs to can be attached, so a group
  // for the other engine is filtered out rather than offered and then refused.
  const engineFamilies = new Set(
    versionsForEngine.map((v) => v.DBParameterGroupFamily),
  )
  const parameterGroupsForEngine = parameterGroups.filter((g) =>
    engineFamilies.has(g.DBParameterGroupFamily),
  )

  // The subnet group pins the VPC, so the security group list narrows to it
  // once one is chosen. Without a group the account's default VPC is resolved
  // server-side and every group stays on offer.
  const subnetGroupVpc = subnetGroups.find(
    (g) => g.DBSubnetGroupName === selectedSubnetGroup,
  )?.VpcId
  const allSecurityGroups = securityGroupsData.SecurityGroups ?? []
  const securityGroups = subnetGroupVpc
    ? allSecurityGroups.filter((g) => g.VpcId === subnetGroupVpc)
    : allSecurityGroups

  const handleEngineChange = (engine: string | null) => {
    setValue("engine", engine ?? "", { shouldValidate: true })
    setValue("engineVersion", "")
    setValue("dbInstanceClass", "")
    setValue("dbParameterGroupName", "")
  }

  const setSecurityGroups = (next: string[]) =>
    setValue("vpcSecurityGroupIds", next, { shouldValidate: true })

  const onSubmit = async (data: CreateDBInstanceFormData) => {
    await createInstance.mutateAsync(data)
    await navigate({ to: "/rds/describe-db-instances" })
  }

  // Both engine images absent is a cluster that cannot run RDS at all, so the
  // form is replaced. One absent is the normal state of a fresh cluster and
  // must leave the other engine's create path intact.
  if (engines.length > 0 && bootableEngines.length === 0) {
    return (
      <>
        <BackLink to="/rds/describe-db-instances">Back to databases</BackLink>
        <PageHeading title="Create database" />
        <SystemImageRequired
          description={`RDS boots a Spinifex-managed engine image that is not shipped with the platform. Import one before creating a database. Engines offered: ${engines.join(", ")}.`}
          importCommand={rdsImportCommand(engines[0] ?? "")}
          isRechecking={isRechecking}
          onRecheck={handleRecheck}
          title="RDS system image not found"
        />
      </>
    )
  }

  // Driven by which engines lack an image rather than by the selection: the
  // picker refuses to select one that lacks it, so a callout waiting on the
  // selection is a callout that never appears.
  const enginesMissingImage = engines.filter((e) => !engineHasImage(e))

  const classNotice: PickerNotice | undefined =
    selectedEngine === ""
      ? {
          tone: "muted",
          text: "Select an engine to see the instance classes this cluster can run.",
        }
      : pickerNotice(orderableQuery, instanceClasses.length === 0, {
          loading: "Loading the instance classes…",
          failed: "Could not read the instance classes",
          empty: `No instance class this cluster's nodes can run is available for ${selectedEngine}.`,
        })

  return (
    <>
      <BackLink to="/rds/describe-db-instances">Back to databases</BackLink>
      <PageHeading title="Create database" />

      {createInstance.error && (
        <ErrorBanner
          error={createInstance.error}
          msg="Failed to create the DB instance"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="db-identifier">DB instance identifier</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.dbInstanceIdentifier}
            id="db-identifier"
            placeholder="orders-db"
            {...register("dbInstanceIdentifier")}
          />
          <FieldDescription>
            Lowercase letters, digits and hyphens. This is also the endpoint
            hostname, so it cannot be changed later.
          </FieldDescription>
          <FieldError errors={[errors.dbInstanceIdentifier]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-engine">Engine</label>
          </FieldTitle>
          <Controller
            control={control}
            name="engine"
            render={({ field }) => (
              <Select onValueChange={handleEngineChange} value={field.value}>
                <SelectTrigger
                  aria-invalid={!!errors.engine}
                  className="w-full"
                  id="db-engine"
                >
                  <SelectValue placeholder="Select an engine" />
                </SelectTrigger>
                <SelectContent>
                  {engines.map((engine) => (
                    <SelectItem
                      disabled={!engineHasImage(engine)}
                      key={engine}
                      value={engine}
                    >
                      {engineHasImage(engine)
                        ? engine
                        : `${engine} — image not imported`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.engine]} />
        </Field>

        {enginesMissingImage.map((engine) => (
          <SystemImageRequired
            description={`No ${engine} image is imported on this cluster, so a ${engine} database cannot launch. The other engines are unaffected.`}
            importCommand={rdsImportCommand(engine)}
            isRechecking={isRechecking}
            key={engine}
            onRecheck={handleRecheck}
            title={`${engine} image not found`}
          />
        ))}

        <Field>
          <FieldTitle>
            <label htmlFor="db-engine-version">Engine version</label>
          </FieldTitle>
          <Controller
            control={control}
            name="engineVersion"
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value}>
                <SelectTrigger className="w-full" id="db-engine-version">
                  <SelectValue placeholder="Engine default" />
                </SelectTrigger>
                <SelectContent>
                  {versionsForEngine.map((version) => (
                    <SelectItem
                      key={version.EngineVersion}
                      value={version.EngineVersion ?? ""}
                    >
                      {version.DBEngineVersionDescription ??
                        version.EngineVersion}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldDescription>
            There is no in-place version upgrade, so this cannot be changed
            after the database is created.
          </FieldDescription>
          <FieldError errors={[errors.engineVersion]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-instance-class">DB instance class</label>
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
                    id="db-instance-class"
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
            <label htmlFor="db-storage">Allocated storage (GiB)</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.allocatedStorage}
            id="db-storage"
            max={storageBounds?.MaxStorageSize}
            min={storageBounds?.MinStorageSize}
            type="number"
            {...register("allocatedStorage", { valueAsNumber: true })}
          />
          <FieldDescription>
            gp3, encrypted. Storage grows only — it can never be shrunk.
            {storageBounds?.MinStorageSize !== undefined &&
              ` This engine accepts ${storageBounds.MinStorageSize}–${storageBounds.MaxStorageSize} GiB.`}
          </FieldDescription>
          <FieldError errors={[errors.allocatedStorage]} />
        </Field>

        <Field>
          <FieldTitle>Storage encryption</FieldTitle>
          <p className="text-sm">Encrypted — always on.</p>
          <FieldDescription>
            Storage is encrypted with the cluster key. Unencrypted storage is
            not offered and a customer-managed key is not selectable.
          </FieldDescription>
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-master-username">Master username</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.masterUsername}
            id="db-master-username"
            placeholder="dbadmin"
            {...register("masterUsername")}
          />
          <FieldError errors={[errors.masterUsername]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-master-password">Master password</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.masterUserPassword}
            id="db-master-password"
            type="password"
            {...register("masterUserPassword")}
          />
          <FieldDescription>
            8–128 printable ASCII characters, excluding &apos;/&apos;,
            &apos;&quot;&apos;, &apos;@&apos; and spaces.
          </FieldDescription>
          <FieldError errors={[errors.masterUserPassword]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-confirm-password">Confirm master password</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.confirmPassword}
            id="db-confirm-password"
            type="password"
            {...register("confirmPassword")}
          />
          <FieldError errors={[errors.confirmPassword]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-name">Initial database name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.dbName}
            id="db-name"
            placeholder="Optional"
            {...register("dbName")}
          />
          <FieldDescription>
            Leave blank to create no database. Letters, digits and underscores,
            starting with a letter.
          </FieldDescription>
          <FieldError errors={[errors.dbName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-port">Port</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.port}
            id="db-port"
            placeholder="Engine default"
            {...register("port")}
          />
          <FieldDescription>
            Leave blank to use the engine&apos;s own port. The port is fixed at
            create and cannot be changed later.
          </FieldDescription>
          <FieldError errors={[errors.port]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-subnet-group">DB subnet group</label>
          </FieldTitle>
          <Controller
            control={control}
            name="dbSubnetGroupName"
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value}>
                <SelectTrigger className="w-full" id="db-subnet-group">
                  <SelectValue placeholder="Default VPC subnet" />
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
            Leave unset to place the endpoint in the account&apos;s default VPC
            subnet. The subnet group cannot be changed after create.
          </FieldDescription>
          <FieldError errors={[errors.dbSubnetGroupName]} />
        </Field>

        <Field>
          <FieldTitle>VPC security groups</FieldTitle>
          <SecurityGroupCheckboxes
            emptyText={`No security groups available${subnetGroupVpc ? ` in ${subnetGroupVpc}` : ""}.`}
            groups={securityGroups}
            onChange={setSecurityGroups}
            selected={selectedSecurityGroups}
          />
          <FieldError errors={[errors.vpcSecurityGroupIds]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-parameter-group">DB parameter group</label>
          </FieldTitle>
          <Controller
            control={control}
            name="dbParameterGroupName"
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value}>
                <SelectTrigger className="w-full" id="db-parameter-group">
                  <SelectValue placeholder="Engine default group" />
                </SelectTrigger>
                <SelectContent>
                  {parameterGroupsForEngine.map((group) => (
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
          <FieldTitle>
            <label htmlFor="db-backup-retention">Backup retention (days)</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.backupRetentionPeriod}
            id="db-backup-retention"
            max={MAX_BACKUP_RETENTION_DAYS}
            min={0}
            type="number"
            {...register("backupRetentionPeriod", { valueAsNumber: true })}
          />
          <FieldDescription>
            0 disables automated backups. There is no point-in-time restore;
            backups are daily snapshots taken in the window below.
          </FieldDescription>
          <FieldError errors={[errors.backupRetentionPeriod]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-backup-window">Preferred backup window</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.preferredBackupWindow}
            id="db-backup-window"
            placeholder="hh24:mi-hh24:mi in UTC"
            {...register("preferredBackupWindow")}
          />
          <FieldDescription>
            Leave blank to have one assigned. Minimum 30 minutes.
          </FieldDescription>
          <FieldError errors={[errors.preferredBackupWindow]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="db-maintenance-window">
              Preferred maintenance window
            </label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.preferredMaintenanceWindow}
            id="db-maintenance-window"
            placeholder="ddd:hh24:mi-ddd:hh24:mi in UTC"
            {...register("preferredMaintenanceWindow")}
          />
          <FieldDescription>
            Leave blank to have one assigned clear of the backup window.
          </FieldDescription>
          <FieldError errors={[errors.preferredMaintenanceWindow]} />
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
          <FieldError errors={[errors.deletionProtection]} />
        </Field>

        <TagsFieldArray control={control} name="tags" />

        <fieldset className="space-y-3 rounded-md border border-border p-4">
          <legend className="px-1 text-sm font-medium">
            Not available on Spinifex
          </legend>
          {UNAVAILABLE_OPTIONS.map((option) => (
            <label
              className="flex items-start gap-2 text-xs text-muted-foreground"
              key={option.id}
            >
              <input checked={false} disabled readOnly type="checkbox" />
              <span>
                {option.label}
                <span className="mt-0.5 block">{option.note}</span>
              </span>
            </label>
          ))}
        </fieldset>

        <CliCommandPanel
          commands={buildCreateDBInstanceCommands({
            dbInstanceIdentifier: values.dbInstanceIdentifier ?? "",
            engine: selectedEngine,
            engineVersion: values.engineVersion ?? "",
            dbInstanceClass: values.dbInstanceClass ?? "",
            allocatedStorage:
              values.allocatedStorage ?? MIN_ALLOCATED_STORAGE_GIB,
            masterUsername: values.masterUsername ?? "",
            dbName: values.dbName ?? "",
            port: values.port ?? "",
            dbSubnetGroupName: selectedSubnetGroup,
            vpcSecurityGroupIds: selectedSecurityGroups,
            dbParameterGroupName: values.dbParameterGroupName ?? "",
            deletionProtection: values.deletionProtection ?? false,
            backupRetentionPeriod:
              values.backupRetentionPeriod ?? DEFAULT_BACKUP_RETENTION_DAYS,
            preferredBackupWindow: values.preferredBackupWindow ?? "",
            preferredMaintenanceWindow: values.preferredMaintenanceWindow ?? "",
          })}
        />

        <FormActions
          isPending={createInstance.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/rds/describe-db-instances" })
          }
          pendingLabel="Creating…"
          submitLabel="Create database"
        />
      </form>
    </>
  )
}

// Everything the CLI panel renders, already flattened out of the form state so
// the builder needs no optional handling of its own.
type CliValues = Omit<
  CreateDBInstanceFormData,
  "masterUserPassword" | "confirmPassword" | "tags"
>

function buildCreateDBInstanceCommands(values: CliValues): CliCommand[] {
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
