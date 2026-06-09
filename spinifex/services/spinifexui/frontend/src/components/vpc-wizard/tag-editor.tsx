import { Plus, Trash2 } from "lucide-react"
import type { UseFormReturn } from "react-hook-form"
import { useWatch } from "react-hook-form"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import type { CreateVpcWizardFormData } from "@/types/ec2"

interface TagEditorProps {
  form: UseFormReturn<CreateVpcWizardFormData>
}

export function TagEditor({ form }: TagEditorProps) {
  const tags = useWatch({ control: form.control, name: "tags" })

  function addTag() {
    form.setValue("tags", [...tags, { key: "", value: "" }])
  }

  function removeTag(index: number) {
    form.setValue(
      "tags",
      tags.filter((_, i) => i !== index),
    )
  }

  return (
    <div className="space-y-2">
      {tags.map((_, index) => (
        // oxlint-disable-next-line react/no-array-index-key -- form array with no stable id
        <div className="flex items-center gap-2" key={index}>
          <Input placeholder="Key" {...form.register(`tags.${index}.key`)} />
          <Input
            placeholder="Value"
            {...form.register(`tags.${index}.value`)}
          />
          <Button
            onClick={() => removeTag(index)}
            size="icon"
            type="button"
            variant="ghost"
          >
            <Trash2 className="size-3.5" />
          </Button>
        </div>
      ))}
      <Button onClick={addTag} size="sm" type="button" variant="outline">
        <Plus className="size-3.5" />
        Add tag
      </Button>
    </div>
  )
}
