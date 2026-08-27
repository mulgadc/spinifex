import {
  ApplyGuardrailCommand,
  type GuardrailContentSource,
} from "@aws-sdk/client-bedrock-runtime"
import { useMutation } from "@tanstack/react-query"

import { getBedrockRuntimeClient } from "@/lib/awsClient"

export interface ApplyGuardrailParams {
  guardrailIdentifier: string
  guardrailVersion: string
  source: GuardrailContentSource
  text: string
}

// ApplyGuardrail has no side effects, but the tester triggers it on demand
// rather than on mount, so it fits the mutation primitive (loading/error/data
// state) better than a query.
export function useApplyGuardrail() {
  return useMutation({
    mutationFn: async ({
      guardrailIdentifier,
      guardrailVersion,
      source,
      text,
    }: ApplyGuardrailParams) => {
      const command = new ApplyGuardrailCommand({
        guardrailIdentifier,
        guardrailVersion,
        source,
        content: [{ text: { text } }],
      })
      return await getBedrockRuntimeClient().send(command)
    },
  })
}
