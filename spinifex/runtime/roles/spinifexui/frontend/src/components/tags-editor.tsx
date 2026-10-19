import { zodResolver } from "@hookform/resolvers/zod"
import { useForm, useWatch } from "react-hook-form"
import { z } from "zod"

import { EditorShell } from "@/components/editor-shell"
import { TagsFieldArray } from "@/components/tags-field-array"
import { type TagFormData, tagSchema } from "@/types/tags"

// Structural rather than one SDK's Tag: ELBv2 and RDS declare the same shape in
// their own packages, and the editor works against either.
interface Tag {
  Key?: string
  Value?: string
}

// A blank row is dropped on save rather than refused, and a value without a key
// is reported across rows, so a row's own key may be empty here.
const tagsEditorSchema = z.object({
  tags: z.array(
    tagSchema.extend({ key: z.literal("").or(tagSchema.shape.key) }),
  ),
})

type TagsEditorFormData = z.infer<typeof tagsEditorSchema>

interface TagsEditorProps {
  tags: Tag[]
  onSubmit: (tags: TagFormData[]) => void
  isPending: boolean
  error?: Error | null
  isSuccess?: boolean
}

function toRows(tags: Tag[]): TagFormData[] {
  return tags.map((t) => ({ key: t.Key ?? "", value: t.Value ?? "" }))
}

function normalize(rows: TagFormData[]): TagFormData[] {
  return rows.flatMap((r) => {
    const key = r.key.trim()

    return key.length > 0 ? [{ key, value: r.value }] : []
  })
}

export function TagsEditor({
  tags,
  onSubmit,
  isPending,
  error,
  isSuccess,
}: TagsEditorProps) {
  const initialRows = toRows(tags)

  const { control, formState, reset } = useForm<TagsEditorFormData>({
    resolver: zodResolver(tagsEditorSchema),
    mode: "onChange",
    defaultValues: { tags: initialRows },
  })

  const rows = useWatch({ control, name: "tags" })

  // A row with a value but no key is incomplete; duplicate keys are ambiguous.
  const hasBlankKey = rows.some(
    (r) => r.key.trim().length === 0 && r.value.length > 0,
  )

  const normalized = normalize(rows)
  const keys = normalized.map((r) => r.key)
  const hasDuplicate = new Set(keys).size !== keys.length
  const hasError = hasBlankKey || hasDuplicate || !formState.isValid

  const dirty =
    JSON.stringify(normalized) !== JSON.stringify(normalize(initialRows))

  return (
    <EditorShell
      dirty={dirty}
      error={error}
      errorMsg="Failed to save tags"
      invalid={hasError}
      isPending={isPending}
      isSuccess={isSuccess}
      onReset={() => {
        reset({ tags: initialRows })
      }}
      onSave={() => {
        onSubmit(normalized)
      }}
      successMsg="Tags saved."
    >
      <div className="flex flex-col gap-2 p-4">
        <TagsFieldArray control={control} name="tags" />
        {hasBlankKey && (
          <p className="text-xs text-destructive">
            Every tag with a value needs a key.
          </p>
        )}
        {hasDuplicate && (
          <p className="text-xs text-destructive">Tag keys must be unique.</p>
        )}
      </div>
    </EditorShell>
  )
}
