import type { ComponentProps, ReactNode } from "react"
import {
  type Control,
  type FieldValues,
  type Path,
  Controller,
} from "react-hook-form"

import { type PickerNotice, PickerNoticeText } from "@/components/picker-notice"
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

export interface SelectFieldOption {
  value: string
  label: string
}

// For pickers whose values are already what the user should read.
export function selectOptions(values: readonly string[]): SelectFieldOption[] {
  return values.map((value) => ({ value, label: value }))
}

export interface SelectFieldProps<T extends FieldValues> {
  control: Control<T>
  name: Path<T>
  id: string
  label: string
  options: SelectFieldOption[]
  placeholder?: string
  // When set, the notice replaces the control: an empty picker has a cause and
  // an inert dropdown does not say what it is.
  notice?: PickerNotice
  description?: ReactNode
  // Rendered under the control, for a change whose cost the user should see
  // before submitting rather than after.
  warning?: ReactNode
  onValueChange?: (value: string) => void
}

// The base-ui Select is not a native input and cannot be wired up with
// `register`, so every form-bound picker goes through this Controller.
export function SelectField<T extends FieldValues>({
  control,
  name,
  id,
  label,
  options,
  placeholder,
  notice,
  description,
  warning,
  onValueChange,
}: SelectFieldProps<T>) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => {
        // react-hook-form cannot narrow a generic path's value, and Select
        // infers its own type from this prop, so it is pinned to a string here.
        const selected: string =
          // oxlint-disable-next-line anti-slop/no-runtime-typeof -- the generic path's value has no narrower type to branch on
          typeof field.value === "string" ? field.value : ""

        // Select reports an explicit null when cleared; every form holds an
        // empty string for "unset".
        const handleChange = (next: string | null) => {
          ;(onValueChange ?? field.onChange)(next ?? "")
        }

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

type TextFieldProps = ComponentProps<typeof Input> & {
  id: string
  label: string
  error?: { message?: string }
  description?: ReactNode
  warning?: ReactNode
}

// Takes the rest of its props straight from `register(...)`, so a text, number
// or password input keeps react-hook-form's native-input wiring.
export function TextField({
  id,
  label,
  error,
  description,
  warning,
  ...inputProps
}: TextFieldProps) {
  return (
    <Field>
      <FieldTitle>
        <label htmlFor={id}>{label}</label>
      </FieldTitle>
      <Input aria-invalid={!!error} id={id} {...inputProps} />
      {description && <FieldDescription>{description}</FieldDescription>}
      {warning && <p className="text-xs text-tactical-amber">{warning}</p>}
      <FieldError errors={[error]} />
    </Field>
  )
}
