import {
  GetGuardrailCommand,
  ListFoundationModelsCommand,
  ListGuardrailsCommand,
} from "@aws-sdk/client-bedrock"
import { queryOptions } from "@tanstack/react-query"

import { getBedrockClient } from "@/lib/awsClient"

const GUARDRAIL_STALE_TIME = 30_000
const FOUNDATION_MODELS_STALE_TIME = 30_000

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

// The tenant view of ListFoundationModels — only models this account can see,
// as opposed to the admin ListOchreCatalog which also reports why an entry is
// unavailable. Self-host only in v1.
export const foundationModelsQueryOptions = queryOptions({
  queryKey: ["bedrock", "foundationModels"],
  queryFn: async () => {
    const command = new ListFoundationModelsCommand({})
    return await getBedrockClient().send(command)
  },
  staleTime: FOUNDATION_MODELS_STALE_TIME,
})
