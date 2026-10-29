import type { OrderableDBInstanceOption } from "@aws-sdk/client-rds"
import {
  type Control,
  type FieldValues,
  type Path,
  Controller,
} from "react-hook-form"

import { pickerNotice } from "@/components/picker-notice"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"

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

// The catalog lists a class once per engine version, so it is offered once.
export function instanceClassOptions(
  query: {
    data?: { OrderableDBInstanceOptions?: OrderableDBInstanceOption[] }
    error: Error | null
    isPending: boolean
  },
  engine: string,
) {
  const instanceClasses = [
    ...new Set(
      (query.data?.OrderableDBInstanceOptions ?? [])
        .map((o) => o.DBInstanceClass ?? "")
        .filter(Boolean),
    ),
  ]

  return {
    instanceClasses,
    classNotice: pickerNotice(query, instanceClasses.length === 0, {
      loading: "Loading the instance classes…",
      failed: "Could not read the instance classes",
      empty: `No instance class this cluster's nodes can run is available for ${engine}.`,
    }),
  }
}
