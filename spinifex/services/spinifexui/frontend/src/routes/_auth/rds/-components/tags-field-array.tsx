import { Plus, Trash2 } from "lucide-react"
import {
  type ArrayPath,
  type Control,
  type FieldArray,
  type FieldValues,
  type Path,
  Controller,
  useFieldArray,
} from "react-hook-form"

import { Button } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

interface TagsFieldArrayProps<T extends FieldValues> {
  control: Control<T>
  name: ArrayPath<T>
}

// The key/value rows every RDS create form carries. Keyed by useFieldArray's
// own id so removing a row does not shuffle the values of the ones below it.
export function TagsFieldArray<T extends FieldValues>({
  control,
  name,
}: TagsFieldArrayProps<T>) {
  const { fields, append, remove } = useFieldArray({ control, name })

  return (
    <Field>
      <FieldTitle>Tags</FieldTitle>
      <div className="space-y-2">
        {fields.map((field, index) => (
          <div className="flex items-center gap-2" key={field.id}>
            <Controller
              control={control}
              // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- react-hook-form cannot infer a path built from a generic array name
              name={`${name}.${index}.key` as Path<T>}
              render={({ field: key }) => <Input placeholder="Key" {...key} />}
            />
            <Controller
              control={control}
              // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- as above
              name={`${name}.${index}.value` as Path<T>}
              render={({ field: value }) => (
                <Input placeholder="Value" {...value} />
              )}
            />
            <Button
              onClick={() => remove(index)}
              size="icon"
              type="button"
              variant="ghost"
            >
              <Trash2 className="size-3.5" />
            </Button>
          </div>
        ))}
        <Button
          onClick={() =>
            // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- every caller's tags array is this row shape
            append({ key: "", value: "" } as FieldArray<T, ArrayPath<T>>)
          }
          size="sm"
          type="button"
          variant="outline"
        >
          <Plus className="size-3.5" />
          Add tag
        </Button>
      </div>
    </Field>
  )
}
