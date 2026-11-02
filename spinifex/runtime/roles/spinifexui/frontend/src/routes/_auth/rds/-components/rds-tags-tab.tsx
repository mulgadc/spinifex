import { useQuery } from "@tanstack/react-query"

import { InlineTags } from "@/components/inline-tags"
import { useRdsTagWriter } from "@/mutations/tags"
import { rdsTagsQueryOptions } from "@/queries/rds"

// Every RDS resource is tagged by ARN through the same calls, so the query and
// the writer live here rather than being restated on each detail page.
export function RdsTagsTab({ arn }: { arn: string }) {
  // Not suspense: the ARN is empty for a resource that has none, and an empty
  // one is a request the tags API refuses.
  const { data } = useQuery(rdsTagsQueryOptions(arn))
  const writer = useRdsTagWriter(arn)

  return <InlineTags tags={data?.TagList} {...writer} />
}
