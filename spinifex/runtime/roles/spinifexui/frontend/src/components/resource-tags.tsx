import type { QueryKey } from "@tanstack/react-query"

import { InlineTags } from "@/components/inline-tags"
import {
  type IamTaggable,
  useEc2TagWriter,
  useEcsTagWriter,
  useEksTagWriter,
  useIamTagWriter,
} from "@/mutations/tags"

interface Tag {
  Key?: string
  Value?: string
}

// Every EC2 resource tags by its id through CreateTags and DeleteTags.
export function Ec2Tags({
  id,
  invalidate,
  tags,
  readOnly = false,
}: {
  id: string | undefined
  invalidate: QueryKey
  tags: readonly Tag[] | undefined
  readOnly?: boolean
}) {
  const writer = useEc2TagWriter(id ?? "", invalidate)

  return readOnly || !id ? (
    <InlineTags tags={tags} />
  ) : (
    <InlineTags tags={tags} {...writer} />
  )
}

export function IamTags({
  kind,
  id,
  tags,
  readOnly = false,
}: {
  kind: IamTaggable
  id: string
  tags: readonly Tag[] | undefined
  readOnly?: boolean
}) {
  const writer = useIamTagWriter(kind, id)

  return readOnly ? (
    <InlineTags tags={tags} />
  ) : (
    <InlineTags tags={tags} {...writer} />
  )
}

// EKS keeps its tags as a map; the editor reads EC2's list.
export function EksTags({
  arn,
  tags,
}: {
  arn: string | undefined
  tags: Record<string, string> | undefined
}) {
  const writer = useEksTagWriter(arn ?? "")

  const list = Object.entries(tags ?? {}).map(([Key, Value]) => ({
    Key,
    Value,
  }))

  return arn ? (
    <InlineTags tags={list} {...writer} />
  ) : (
    <InlineTags tags={list} />
  )
}

// ECS spells its tag fields in lower case.
export function EcsTags({
  arn,
  tags,
}: {
  arn: string | undefined
  tags: readonly { key?: string; value?: string }[] | undefined
}) {
  const writer = useEcsTagWriter(arn ?? "")
  const list = tags?.map((tag) => ({ Key: tag.key, Value: tag.value }))

  return arn ? (
    <InlineTags tags={list} {...writer} />
  ) : (
    <InlineTags tags={list} />
  )
}
