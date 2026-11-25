import type { ReactNode } from "react"
import {
  type Control,
  type FieldValues,
  type Path,
  Controller,
} from "react-hook-form"

import { FieldLabel } from "@/components/form-fields"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldError } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

interface ColorFieldProps<T extends FieldValues> {
  control: Control<T>
  name: Path<T>
  id: string
  label: string
  // Shown in the picker while the field is empty, which means "use the default".
  placeholder: string
  description?: ReactNode
  info?: ReactNode
  // Rendered under the control, such as the result of a contrast check.
  children?: ReactNode
}

// A #RRGGBB colour, picked or typed. Empty means the default colour, which the
// clear button restores.
export function ColorField<T extends FieldValues>({
  control,
  name,
  id,
  label,
  placeholder,
  description,
  info,
  children,
}: ColorFieldProps<T>) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field, fieldState }) => {
        const value: string =
          // oxlint-disable-next-line anti-slop/no-runtime-typeof -- the generic path's value has no narrower type to branch on
          typeof field.value === "string" ? field.value : ""

        return (
          <Field>
            <FieldLabel id={id} info={info} label={label} />
            <div className="flex items-center gap-2">
              <input
                aria-label={`${label} picker`}
                className="h-7 w-9 shrink-0 cursor-pointer rounded-md border bg-transparent"
                onChange={(event) => {
                  field.onChange(event.target.value.toUpperCase())
                }}
                type="color"
                value={value || placeholder}
              />
              <Input
                aria-invalid={!!fieldState.error}
                variant="code"
                id={id}
                maxLength={7}
                onBlur={field.onBlur}
                onChange={(event) => {
                  field.onChange(event.target.value.trim())
                }}
                placeholder="Default"
                value={value}
              />
              <Button
                disabled={value === ""}
                onClick={() => {
                  field.onChange("")
                }}
                type="button"
                variant="outline"
              >
                Use default
              </Button>
            </div>
            {description && <FieldDescription>{description}</FieldDescription>}
            {children}
            <FieldError errors={[fieldState.error]} />
          </Field>
        )
      }}
    />
  )
}
