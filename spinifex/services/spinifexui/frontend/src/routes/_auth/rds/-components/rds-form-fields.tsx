import type { ReactNode } from "react"
import {
  type Control,
  type FieldValues,
  type Path,
  Controller,
} from "react-hook-form"

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

import { type PickerNotice, PickerNoticeText } from "./picker-notice"

export interface RdsSelectOption {
  value: string
  label: string
}

interface RdsSelectFieldProps<T extends FieldValues> {
  control: Control<T>
  name: Path<T>
  id: string
  label: string
  placeholder: string
  options: RdsSelectOption[]
  // When set, the notice replaces the control: an empty picker has a cause and
  // an inert dropdown does not say what it is.
  notice?: PickerNotice
  description?: ReactNode
  // Rendered under the control, for a change whose cost the user should see
  // before submitting rather than after.
  warning?: ReactNode
  onValueChange?: (value: string) => void
}

// The instance class, subnet group and parameter group pickers are the same
// control over different options, and the create, restore and modify forms all
// carry them. One component keeps their behaviour from drifting apart.
export function RdsSelectField<T extends FieldValues>({
  control,
  name,
  id,
  label,
  placeholder,
  options,
  notice,
  description,
  warning,
  onValueChange,
}: RdsSelectFieldProps<T>) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => {
        // react-hook-form cannot narrow a generic path's value, and Select
        // infers its own type from this prop, so it is pinned to a string here.
        const selected: string =
          typeof field.value === "string" ? field.value : ""
        // Select reports an explicit null when cleared; every form below holds
        // an empty string for "unset".
        const handleChange = (next: string | null) =>
          (onValueChange ?? field.onChange)(next ?? "")

        return (
          <Field>
            <FieldTitle>
              <label htmlFor={id}>{label}</label>
            </FieldTitle>
            {notice ? (
              <PickerNoticeText notice={notice} />
            ) : (
              <Select onValueChange={handleChange} value={selected}>
                <SelectTrigger
                  aria-invalid={!!fieldState.error}
                  className="w-full"
                  id={id}
                >
                  <SelectValue placeholder={placeholder} />
                </SelectTrigger>
                <SelectContent>
                  {options.map((option) => (
                    <SelectItem key={option.value} value={option.value}>
                      {option.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
            {description && <FieldDescription>{description}</FieldDescription>}
            {warning && (
              <p className="text-xs text-tactical-amber">{warning}</p>
            )}
            <FieldError errors={[fieldState.error]} />
          </Field>
        )
      }}
    />
  )
}

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
          <label className="flex items-center gap-2 text-xs">
            <input
              aria-label="Enable deletion protection"
              checked={field.value}
              onChange={(e) => field.onChange(e.target.checked)}
              type="checkbox"
            />
            <span>Refuse DeleteDBInstance while this is on</span>
          </label>
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
    versions
      .filter((v) => v.Engine === engine)
      .map((v) => v.DBParameterGroupFamily),
  )
  return groups.filter((g) => families.has(g.DBParameterGroupFamily))
}
