import { RetrieveCommand } from "@aws-sdk/client-bedrock-agent-runtime"
import { useMutation } from "@tanstack/react-query"

import { getBedrockAgentRuntimeClient } from "@/lib/awsClient"

export interface RetrieveParams {
  knowledgeBaseId: string
  query: string
}

// Retrieve has no side effects, but the tester triggers it on demand rather
// than on mount, so it fits the mutation primitive (loading/error/data state)
// better than a query.
export function useRetrieve() {
  return useMutation({
    mutationFn: async ({ knowledgeBaseId, query }: RetrieveParams) => {
      const command = new RetrieveCommand({
        knowledgeBaseId,
        retrievalQuery: { text: query },
      })
      return await getBedrockAgentRuntimeClient().send(command)
    },
  })
}
