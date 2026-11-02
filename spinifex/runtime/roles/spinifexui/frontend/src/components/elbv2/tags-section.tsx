import { useSuspenseQuery } from "@tanstack/react-query"

import { TagsEditor } from "@/components/tags-editor"
import { useUpdateTags } from "@/mutations/elbv2"
import { elbv2TagsQueryOptions } from "@/queries/elbv2"

// ELBv2 tags live behind their own call, keyed by ARN, so the editor loads them.
export function Elbv2TagsSection({ arn }: { arn: string }) {
  const { data } = useSuspenseQuery(elbv2TagsQueryOptions([arn]))
  const updateTags = useUpdateTags()

  const tags =
    data.TagDescriptions?.find((td) => td.ResourceArn === arn)?.Tags ?? []

  return (
    <TagsEditor
      error={updateTags.error}
      isPending={updateTags.isPending}
      isSuccess={updateTags.isSuccess}
      onSubmit={(next) => {
        updateTags.mutate({
          resourceArn: arn,
          tags: next,
          initialKeys: tags.map((t) => t.Key ?? "").filter((k) => k.length > 0),
        })
      }}
      tags={tags}
    />
  )
}
