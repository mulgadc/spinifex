import {
  CreateTagsCommand as Ec2CreateTagsCommand,
  DeleteTagsCommand as Ec2DeleteTagsCommand,
} from "@aws-sdk/client-ec2"
import {
  TagResourceCommand as EcsTagResourceCommand,
  UntagResourceCommand as EcsUntagResourceCommand,
} from "@aws-sdk/client-ecs"
import {
  TagResourceCommand as EksTagResourceCommand,
  UntagResourceCommand as EksUntagResourceCommand,
} from "@aws-sdk/client-eks"
import {
  AddTagsCommand as Elbv2AddTagsCommand,
  RemoveTagsCommand as Elbv2RemoveTagsCommand,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import {
  TagInstanceProfileCommand,
  TagPolicyCommand,
  TagRoleCommand,
  TagUserCommand,
  UntagInstanceProfileCommand,
  UntagPolicyCommand,
  UntagRoleCommand,
  UntagUserCommand,
} from "@aws-sdk/client-iam"
import {
  AddTagsToResourceCommand,
  RemoveTagsFromResourceCommand,
} from "@aws-sdk/client-rds"
import { type QueryKey, useQueryClient } from "@tanstack/react-query"

import {
  getEc2Client,
  getEcsClient,
  getEksClient,
  getElbv2Client,
  getIamClient,
  getRdsClient,
} from "@/lib/awsClient"

export interface TagWriter {
  onPut: (key: string, value: string) => Promise<void>
  onRemove: (key: string) => Promise<void>
}

// The calls' outputs carry nothing the editor reads.
type Send = Promise<object>

// Each service tags through its own pair of calls; the editor sees one shape,
// and every write refreshes the queries that carry the resource's tags.
function useTagWriter(
  invalidate: QueryKey,
  put: (key: string, value: string) => Send,
  remove: (key: string) => Send,
): TagWriter {
  const queryClient = useQueryClient()

  async function then(work: Send) {
    await work
    await queryClient.invalidateQueries({ queryKey: invalidate })
  }

  return {
    onPut: async (key, value) => {
      await then(put(key, value))
    },
    onRemove: async (key) => {
      await then(remove(key))
    },
  }
}

// `invalidate` is the resource's list key, which prefixes its detail key too.
export function useEc2TagWriter(
  resourceId: string,
  invalidate: QueryKey,
): TagWriter {
  return useTagWriter(
    invalidate,
    async (key, value) =>
      await getEc2Client().send(
        new Ec2CreateTagsCommand({
          Resources: [resourceId],
          Tags: [{ Key: key, Value: value }],
        }),
      ),
    async (key) =>
      await getEc2Client().send(
        new Ec2DeleteTagsCommand({
          Resources: [resourceId],
          Tags: [{ Key: key }],
        }),
      ),
  )
}

export function useElbv2TagWriter(arn: string): TagWriter {
  return useTagWriter(
    ["elbv2", "tags"],
    async (key, value) =>
      await getElbv2Client().send(
        new Elbv2AddTagsCommand({
          ResourceArns: [arn],
          Tags: [{ Key: key, Value: value }],
        }),
      ),
    async (key) =>
      await getElbv2Client().send(
        new Elbv2RemoveTagsCommand({ ResourceArns: [arn], TagKeys: [key] }),
      ),
  )
}

export function useRdsTagWriter(arn: string): TagWriter {
  return useTagWriter(
    ["rds", "tags"],
    async (key, value) =>
      await getRdsClient().send(
        new AddTagsToResourceCommand({
          ResourceName: arn,
          Tags: [{ Key: key, Value: value }],
        }),
      ),
    async (key) =>
      await getRdsClient().send(
        new RemoveTagsFromResourceCommand({
          ResourceName: arn,
          TagKeys: [key],
        }),
      ),
  )
}

export function useEksTagWriter(arn: string): TagWriter {
  return useTagWriter(
    ["eks", "clusters"],
    async (key, value) =>
      await getEksClient().send(
        new EksTagResourceCommand({ resourceArn: arn, tags: { [key]: value } }),
      ),
    async (key) =>
      await getEksClient().send(
        new EksUntagResourceCommand({ resourceArn: arn, tagKeys: [key] }),
      ),
  )
}

export function useEcsTagWriter(arn: string): TagWriter {
  return useTagWriter(
    ["ecs", "clusters"],
    async (key, value) =>
      await getEcsClient().send(
        new EcsTagResourceCommand({
          resourceArn: arn,
          tags: [{ key, value }],
        }),
      ),
    async (key) =>
      await getEcsClient().send(
        new EcsUntagResourceCommand({ resourceArn: arn, tagKeys: [key] }),
      ),
  )
}

export type IamTaggable = "user" | "role" | "policy" | "instance-profile"

const IAM_LIST_KEYS: Record<IamTaggable, QueryKey> = {
  user: ["iam", "users"],
  role: ["iam", "roles"],
  policy: ["iam", "policies"],
  "instance-profile": ["iam", "instance-profiles"],
}

interface IamCalls {
  put: (id: string, key: string, value: string) => Send
  remove: (id: string, key: string) => Send
}

const IAM_CALLS: Record<IamTaggable, IamCalls> = {
  user: {
    put: async (UserName, Key, Value) =>
      await getIamClient().send(
        new TagUserCommand({ UserName, Tags: [{ Key, Value }] }),
      ),
    remove: async (UserName, key) =>
      await getIamClient().send(
        new UntagUserCommand({ UserName, TagKeys: [key] }),
      ),
  },
  role: {
    put: async (RoleName, Key, Value) =>
      await getIamClient().send(
        new TagRoleCommand({ RoleName, Tags: [{ Key, Value }] }),
      ),
    remove: async (RoleName, key) =>
      await getIamClient().send(
        new UntagRoleCommand({ RoleName, TagKeys: [key] }),
      ),
  },
  policy: {
    put: async (PolicyArn, Key, Value) =>
      await getIamClient().send(
        new TagPolicyCommand({ PolicyArn, Tags: [{ Key, Value }] }),
      ),
    remove: async (PolicyArn, key) =>
      await getIamClient().send(
        new UntagPolicyCommand({ PolicyArn, TagKeys: [key] }),
      ),
  },
  "instance-profile": {
    put: async (InstanceProfileName, Key, Value) =>
      await getIamClient().send(
        new TagInstanceProfileCommand({
          InstanceProfileName,
          Tags: [{ Key, Value }],
        }),
      ),
    remove: async (InstanceProfileName, key) =>
      await getIamClient().send(
        new UntagInstanceProfileCommand({
          InstanceProfileName,
          TagKeys: [key],
        }),
      ),
  },
}

// `id` is the name, or the ARN for a managed policy.
export function useIamTagWriter(kind: IamTaggable, id: string): TagWriter {
  const calls = IAM_CALLS[kind]

  return useTagWriter(
    IAM_LIST_KEYS[kind],
    async (key, value) => await calls.put(id, key, value),
    async (key) => await calls.remove(id, key),
  )
}
