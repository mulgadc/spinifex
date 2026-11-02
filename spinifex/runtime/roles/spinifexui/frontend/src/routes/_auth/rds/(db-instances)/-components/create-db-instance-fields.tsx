import type { Image, SecurityGroup } from "@aws-sdk/client-ec2"
import type {
  DBEngineVersion,
  OrderableDBInstanceOption,
} from "@aws-sdk/client-rds"
import type { ComponentProps } from "react"
import {
  type Control,
  type DeepPartialSkipArrayKey,
  type FieldError as HookFieldError,
  Controller,
} from "react-hook-form"

import { TextField } from "@/components/form-fields"
import type { PickerNotice } from "@/components/picker-notice"
import { SecurityGroupPicker } from "@/components/security-group-picker"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { isRdsSystemImage } from "@/lib/system-managed"
import {
  type CreateDBInstanceFormData,
  MIN_ALLOCATED_STORAGE_GIB,
} from "@/types/rds"

// The retention the backend applies when a create names none.
export const DEFAULT_BACKUP_RETENTION_DAYS = 7

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
      <FieldTitle>
        <label htmlFor="db-engine">Engine</label>
      </FieldTitle>
      <Controller
        control={control}
        name="engine"
        render={({ field }) => (
          <Select
            onValueChange={(value) => {
              onEngineChange(value ?? "")
            }}
            value={field.value}
          >
            <SelectTrigger
              aria-invalid={!!error}
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
      <FieldDescription>
        There is no in-place version upgrade, so this cannot be changed after
        the database is created.
      </FieldDescription>
      <FieldError errors={[error]} />
    </Field>
  )
}

export function StorageEncryptionField() {
  return (
    <Field>
      <FieldTitle>Storage encryption</FieldTitle>
      <p className="text-sm">Encrypted — always on.</p>
      <FieldDescription>
        Storage is encrypted with the cluster key. Unencrypted storage is not
        offered and a customer-managed key is not selectable.
      </FieldDescription>
    </Field>
  )
}

export function UnavailableOptionsFieldset() {
  return (
    <fieldset className="space-y-3 rounded-md border border-border p-4">
      <legend className="px-1 text-sm font-medium">
        Not available on Spinifex
      </legend>
      {UNAVAILABLE_OPTIONS.map((option) => (
        <div
          className="flex items-start gap-2 text-xs text-muted-foreground"
          key={option.id}
        >
          <span className="mt-0.5 flex">
            <Switch aria-label={option.label} checked={false} disabled />
          </span>
          <span>
            {option.label}
            <span className="mt-0.5 block">{option.note}</span>
          </span>
        </div>
      ))}
    </fieldset>
  )
}

export function AllocatedStorageField({
  bounds,
  ...inputProps
}: Omit<
  ComponentProps<typeof TextField>,
  "description" | "id" | "label" | "max" | "min" | "type"
> & {
  bounds: OrderableDBInstanceOption | undefined
}) {
  return (
    <TextField
      description={
        <>
          gp3, encrypted. Storage grows only — it can never be shrunk.
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
