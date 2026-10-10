import { Plus, Trash2 } from "lucide-react"
import type { UseFormReturn } from "react-hook-form"
import { useFieldArray } from "react-hook-form"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import type { CreateVpcWizardFormData } from "@/types/ec2"

interface TagEditorProps {
  form: UseFormReturn<CreateVpcWizardFormData>
}

export function TagEditor({ form }: TagEditorProps) {
  // The rows are keyed by the field array's own id rather than the index so
  // removing a row does not leave the inputs below it showing the previous
  // row's registered values.
  const { fields, append, remove } = useFieldArray({
    control: form.control,
    name: "tags",
  })

  return (
    <div className="space-y-2">
      {fields.map((field, index) => (
        <div className="flex items-center gap-2" key={field.id}>
          <Input placeholder="Key" {...form.register(`tags.${index}.key`)} />
          <Input
            placeholder="Value"
            {...form.register(`tags.${index}.value`)}
          />
          <Button
            onClick={() => {
              remove(index)
            }}
            size="icon"
            type="button"
            variant="ghost"
          >
            <Trash2 className="size-3.5" />
            <span className="sr-only">Remove tag</span>
          </Button>
        </div>
      ))}
      <Button
        onClick={() => {
          append({ key: "", value: "" })
        }}
        size="sm"
        type="button"
        variant="outline"
      >
        <Plus className="size-3.5" />
        Add tag
      </Button>
    </div>
  )
}
