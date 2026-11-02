import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"
import type {
  DBParameterGroup,
  DBSubnetGroup,
  OrderableDBInstanceOption,
} from "@aws-sdk/client-rds"
import { useQuery } from "@tanstack/react-query"
import {
  type Control,
  type FieldValues,
  type Path,
  Controller,
} from "react-hook-form"

import type { SelectFieldOption } from "@/components/form-fields"
import { pickerNotice } from "@/components/picker-notice"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { describeClassSize } from "@/lib/instance-type-size"
import { ec2InstanceTypesQueryOptions } from "@/queries/ec2"

// Offered identically wherever an instance is created, restored or modified.
export function DeletionProtectionField<T extends FieldValues>({
  control,
  name,
}: {
  control: Control<T>
  name: Path<T>
}) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => (
        <Field>
          <FieldTitle>Deletion protection</FieldTitle>
          <div className="flex items-center gap-2 text-xs">
            <Switch
              aria-label="Enable deletion protection"
              checked={field.value}
              id={name}
              onCheckedChange={field.onChange}
            />
            <Label htmlFor={name}>
              Refuse DeleteDBInstance while this is on
            </Label>
          </div>
          <FieldError errors={[fieldState.error]} />
        </Field>
      )}
    />
  )
}

// Only a group of the instance's own engine family can be attached, so one for
// the other engine is filtered out rather than offered and then refused.
export function parameterGroupsForEngine<
  G extends { DBParameterGroupFamily?: string },
>(
  groups: G[],
  versions: { Engine?: string; DBParameterGroupFamily?: string }[],
  engine: string,
): G[] {
  const families = new Set(
    versions.flatMap((v) =>
      v.Engine === engine ? [v.DBParameterGroupFamily] : [],
    ),
  )

  return groups.filter((g) => families.has(g.DBParameterGroupFamily))
}

type OrderableQuery = Parameters<typeof instanceClassOptions>[0]

// The class options sized by the EC2 type each class runs on.
export function useInstanceClassOptions(query: OrderableQuery, engine: string) {
  const types = useQuery(ec2InstanceTypesQueryOptions).data?.InstanceTypes

  return instanceClassOptions(query, engine, types)
}

// The catalog lists a class once per engine version, so it is offered once.
export function instanceClassOptions(
  query: {
    data?: { OrderableDBInstanceOptions?: OrderableDBInstanceOption[] }
    error: Error | null
    isPending: boolean
  },
  engine: string,
  types: readonly InstanceTypeInfo[] = [],
) {
  const instanceClasses = [
    ...new Set(
      (query.data?.OrderableDBInstanceOptions ?? [])
        .map((o) => o.DBInstanceClass ?? "")
        .filter(Boolean),
    ),
  ]

  const classOptions: SelectFieldOption[] = instanceClasses.map((c) => ({
    value: c,
    label: c,
    detail: describeClassSize(c, types),
  }))

  return {
    instanceClasses,
    classOptions,
    classSize: (c: string | undefined) =>
      c ? describeClassSize(c, types) : undefined,
    classNotice: pickerNotice(query, instanceClasses.length === 0, {
      loading: "Loading the instance classes…",
      failed: "Could not read the instance classes",
      empty: `No instance class this cluster's nodes can run is available for ${engine}.`,
    }),
  }
}

export function subnetGroupOptions(
  groups: DBSubnetGroup[],
): { value: string; label: string }[] {
  return groups.map((g) => ({
    value: g.DBSubnetGroupName ?? "",
    label: `${g.DBSubnetGroupName} (${g.VpcId})`,
  }))
}

export function parameterGroupOptions(
  groups: DBParameterGroup[],
): { value: string; label: string }[] {
  return groups.map((g) => ({
    value: g.DBParameterGroupName ?? "",
    label: g.DBParameterGroupName ?? "",
  }))
}

// A boxed list of the settings a form cannot change, each with the reason.
export function FieldNotes({
  className,
  entries,
  title,
}: {
  className: string
  entries: { label: string; note: string }[]
  title: string
}) {
  return (
    <div className={className}>
      <h3 className="text-xs font-medium">{title}</h3>
      {entries.map((entry) => (
        <p className="text-xs text-muted-foreground" key={entry.label}>
          <span className="font-medium">{entry.label}</span> — {entry.note}
        </p>
      ))}
    </div>
  )
}
