import {
  GetGuardrailCommand,
  ListGuardrailsCommand,
} from "@aws-sdk/client-bedrock"
import { queryOptions } from "@tanstack/react-query"

import { getBedrockClient } from "@/lib/awsClient"

const GUARDRAIL_STALE_TIME = 30_000

export const guardrailsQueryOptions = queryOptions({
  queryKey: ["bedrock", "guardrails"],
  queryFn: async () => {
    const command = new ListGuardrailsCommand({})
    return await getBedrockClient().send(command)
  },
  staleTime: GUARDRAIL_STALE_TIME,
})

export const guardrailQueryOptions = (guardrailIdentifier: string) =>
  queryOptions({
    queryKey: ["bedrock", "guardrails", guardrailIdentifier],
    queryFn: async () => {
      const command = new GetGuardrailCommand({ guardrailIdentifier })
      return await getBedrockClient().send(command)
    },
    staleTime: GUARDRAIL_STALE_TIME,
  })

// ListGuardrails scoped to a single guardrail identifier returns its
// versions (including DRAFT) instead of the account-wide guardrail list.
export const guardrailVersionsQueryOptions = (guardrailIdentifier: string) =>
  queryOptions({
    queryKey: ["bedrock", "guardrails", guardrailIdentifier, "versions"],
    queryFn: async () => {
      const command = new ListGuardrailsCommand({ guardrailIdentifier })
      return await getBedrockClient().send(command)
    },
    staleTime: GUARDRAIL_STALE_TIME,
  })
