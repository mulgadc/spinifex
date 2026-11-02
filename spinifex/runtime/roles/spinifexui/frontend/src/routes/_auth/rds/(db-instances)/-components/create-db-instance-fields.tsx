import type { Image, SecurityGroup } from "@aws-sdk/client-ec2"
import type {
  DBEngineVersion,
  OrderableDBInstanceOption,
} from "@aws-sdk/client-rds"
import { Database } from "lucide-react"
import type { ComponentProps } from "react"
import {
  type Control,
  type DeepPartialSkipArrayKey,
  type FieldError as HookFieldError,
  Controller,
} from "react-hook-form"

import mariadbMark from "@/assets/engines/mariadb.svg?no-inline"
import postgresMark from "@/assets/engines/postgresql.svg?no-inline"
import { ChoiceTile, ChoiceTiles } from "@/components/choice-tile"
import { FieldLabel, TextField } from "@/components/form-fields"
import type { PickerNotice } from "@/components/picker-notice"
import { SecurityGroupPicker } from "@/components/security-group-picker"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { isRdsSystemImage } from "@/lib/system-managed"
import {
  type CreateDBInstanceFormData,
  MIN_ALLOCATED_STORAGE_GIB,
} from "@/types/rds"

// The retention the backend applies when a create names none.
export const DEFAULT_BACKUP_RETENTION_DAYS = 7

// The engine's own mark, served from the console's origin for the CSP's
// img-src 'self'. Both marks are dark, so they sit on a light tile in dark mode.
const ENGINE_MARKS = new Map<string, { name: string; mark: string }>([
  ["mariadb", { name: "MariaDB", mark: mariadbMark }],
  ["postgres", { name: "PostgreSQL", mark: postgresMark }],
])

export function engineName(engine: string): string {
  return ENGINE_MARKS.get(engine)?.name ?? engine
}

function EngineMark({ engine }: { engine: string }) {
  const known = ENGINE_MARKS.get(engine)

  if (!known) {
    return <Database aria-hidden className="size-8 text-muted-foreground" />
  }

  return (
    <img
      alt=""
      className="size-8 object-contain dark:rounded-md dark:bg-white dark:p-0.5"
      data-engine={engine}
      src={known.mark}
    />
  )
}

// Which engines and versions this cluster holds an imported image for.
export function engineImageCatalog(
  engineVersions: DBEngineVersion[],
  images: Image[],
) {
  const engines = [
    ...new Set(engineVersions.flatMap((v) => (v.Engine ? [v.Engine] : []))),
  ]

  const engineVersionHasImage = (engine: string, version: string) =>
    images.some((image) => isRdsSystemImage(image, engine, version))

  const firstBootableVersion = (engine: string) =>
    engineVersions.find(
      (version) =>
        version.Engine === engine &&
        engineVersionHasImage(engine, version.EngineVersion ?? ""),
    )?.EngineVersion ?? ""

  const engineHasImage = (engine: string) =>
    firstBootableVersion(engine).length > 0

  return {
    engines,
    bootableEngines: engines.filter(engineHasImage),
    engineVersionHasImage,
    firstBootableVersion,
    engineHasImage,
  }
}

export function instanceClassNotice(
  selectedEngine: string,
  engineClassNotice: PickerNotice | undefined,
): PickerNotice | undefined {
  return selectedEngine === ""
    ? {
        tone: "muted",
        text: "Select an engine to see the instance classes this cluster can run.",
      }
    : engineClassNotice
}

// Everything the CLI panel renders, already flattened out of the form state so
// the builder needs no optional handling of its own.
export type CreateCliValues = Omit<
  CreateDBInstanceFormData,
  "masterUserPassword" | "confirmPassword" | "tags"
>

export function createCliValues(
  values: DeepPartialSkipArrayKey<CreateDBInstanceFormData>,
): CreateCliValues {
  return {
    dbInstanceIdentifier: values.dbInstanceIdentifier ?? "",
    engine: values.engine ?? "",
    engineVersion: values.engineVersion ?? "",
    dbInstanceClass: values.dbInstanceClass ?? "",
    allocatedStorage: values.allocatedStorage ?? MIN_ALLOCATED_STORAGE_GIB,
    masterUsername: values.masterUsername ?? "",
    dbName: values.dbName ?? "",
    port: values.port ?? "",
    dbSubnetGroupName: values.dbSubnetGroupName ?? "",
    vpcSecurityGroupIds: values.vpcSecurityGroupIds ?? [],
    dbParameterGroupName: values.dbParameterGroupName ?? "",
    deletionProtection: values.deletionProtection ?? false,
    backupRetentionPeriod:
      values.backupRetentionPeriod ?? DEFAULT_BACKUP_RETENTION_DAYS,
    preferredBackupWindow: values.preferredBackupWindow ?? "",
    preferredMaintenanceWindow: values.preferredMaintenanceWindow ?? "",
  }
}

export function EngineField({
  control,
  engines,
  engineHasImage,
  error,
  onEngineChange,
}: {
  control: Control<CreateDBInstanceFormData>
  engines: string[]
  engineHasImage: (engine: string) => boolean
  error: HookFieldError | undefined
  onEngineChange: (engine: string) => void
}) {
  return (
    <Field>
      <FieldTitle>Engine</FieldTitle>
      <Controller
        control={control}
        name="engine"
        render={({ field }) => (
          <ChoiceTiles columns={2} label="Engine">
            {engines.map((engine) => (
              <ChoiceTile
                checked={field.value === engine}
                description={
                  engineHasImage(engine) ? undefined : "Image not imported"
                }
                icon={<EngineMark engine={engine} />}
                key={engine}
                label={engineName(engine)}
                layout="inline"
                onSelect={
                  engineHasImage(engine)
                    ? () => {
                        onEngineChange(engine)
                      }
                    : undefined
                }
              />
            ))}
          </ChoiceTiles>
        )}
      />
      <FieldError errors={[error]} />
    </Field>
  )
}

export function EngineVersionField({
  control,
  versions,
  selectedEngine,
  engineVersionHasImage,
  error,
}: {
  control: Control<CreateDBInstanceFormData>
  versions: DBEngineVersion[]
  selectedEngine: string
  engineVersionHasImage: (engine: string, version: string) => boolean
  error: HookFieldError | undefined
}) {
  return (
    <Field>
      <FieldLabel
        id="db-engine-version"
        info="There is no in-place version upgrade, so the version cannot be changed after the database is created."
        label="Engine version"
      />
      <Controller
        control={control}
        name="engineVersion"
        render={({ field }) => (
          <Select onValueChange={field.onChange} value={field.value}>
            <SelectTrigger className="w-full" id="db-engine-version">
              <SelectValue placeholder="Engine default" />
            </SelectTrigger>
            <SelectContent>
              {versions.map((version) => (
                <SelectItem
                  disabled={
                    !engineVersionHasImage(
                      selectedEngine,
                      version.EngineVersion ?? "",
                    )
                  }
                  key={version.EngineVersion}
                  value={version.EngineVersion ?? ""}
                >
                  {version.DBEngineVersionDescription ?? version.EngineVersion}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      />
      <FieldError errors={[error]} />
    </Field>
  )
}

export function AllocatedStorageField({
  bounds,
  ...inputProps
}: Omit<
  ComponentProps<typeof TextField>,
  "description" | "id" | "info" | "label" | "max" | "min" | "type"
> & {
  bounds: OrderableDBInstanceOption | undefined
}) {
  return (
    <TextField
      info={
        <>
          gp3, encrypted with the cluster key. Storage grows only; it can never
          be shrunk.
          {bounds?.MinStorageSize !== undefined &&
            ` This engine accepts ${bounds.MinStorageSize}–${bounds.MaxStorageSize} GiB.`}
        </>
      }
      id="db-storage"
      label="Allocated storage (GiB)"
      max={bounds?.MaxStorageSize}
      min={bounds?.MinStorageSize}
      type="number"
      {...inputProps}
    />
  )
}

export function PlacementSecurityGroupsField({
  placementVpcId,
  groups,
  onChange,
  selected,
  error,
}: {
  placementVpcId: string | undefined
  groups: SecurityGroup[]
  onChange: (next: string[]) => void
  selected: string[]
  error: { message?: string } | undefined
}) {
  return (
    <Field>
      <FieldTitle>VPC security groups</FieldTitle>
      <SecurityGroupPicker
        emptyText={
          placementVpcId
            ? `No security groups available in ${placementVpcId}.`
            : "No default VPC is available for security groups."
        }
        groups={groups}
        onChange={onChange}
        selected={selected}
      />
      <FieldError errors={[error]} />
    </Field>
  )
}
