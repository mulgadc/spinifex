import { createFileRoute } from "@tanstack/react-router"

import {
  foundationModelsQueryOptions,
  guardrailsQueryOptions,
} from "@/queries/bedrock"

import { PlaygroundPage } from "../-components/playground-page"

export const Route = createFileRoute("/_auth/bedrock/(playground)/playground/")(
  {
    loader: async ({ context }) => {
      await Promise.all([
        context.queryClient.ensureQueryData(foundationModelsQueryOptions),
        context.queryClient.ensureQueryData(guardrailsQueryOptions),
      ])
    },
    head: () => ({
      meta: [{ title: "Playground | Ochre | Mulga" }],
    }),
    component: PlaygroundPage,
  },
)
