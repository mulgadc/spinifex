import { useQuery } from "@tanstack/react-query"

import { TagsEditor } from "@/components/elbv2/tags-editor"
import { useUpdateRdsTags } from "@/mutations/rds"
import { rdsTagsQueryOptions } from "@/queries/rds"

// Every RDS resource is tagged by ARN through the same three actions, so the
// query, the mutation and the key reconciliation live here rather than being
// restated on each detail page.
export function RdsTagsTab({ arn }: { arn: string }) {
  // Not suspense: the ARN is empty for a resource that has none, and an empty
  // one is a request the tags API refuses.
  const { data } = useQuery(rdsTagsQueryOptions(arn))
  const updateTags = useUpdateRdsTags()
  const tags = data?.TagList ?? []

  return (
    <TagsEditor
      error={updateTags.error}
      isPending={updateTags.isPending}
      isSuccess={updateTags.isSuccess}
      onSubmit={(next) =>
        updateTags.mutate({
          resourceName: arn,
          tags: next,
          initialKeys: tags.map((t) => t.Key ?? "").filter((k) => k.length > 0),
        })
      }
      tags={tags}
    />
  )
}
