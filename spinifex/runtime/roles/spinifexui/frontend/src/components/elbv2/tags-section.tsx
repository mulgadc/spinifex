import { useSuspenseQuery } from "@tanstack/react-query"

import { InlineTags } from "@/components/inline-tags"
import { useElbv2TagWriter } from "@/mutations/tags"
import { elbv2TagsQueryOptions } from "@/queries/elbv2"

// ELBv2 tags live behind their own call, keyed by ARN, so the editor loads them.
export function Elbv2TagsSection({ arn }: { arn: string }) {
  const { data } = useSuspenseQuery(elbv2TagsQueryOptions([arn]))
  const writer = useElbv2TagWriter(arn)

  const tags = data.TagDescriptions?.find((td) => td.ResourceArn === arn)?.Tags

  return <InlineTags tags={tags} {...writer} />
}
