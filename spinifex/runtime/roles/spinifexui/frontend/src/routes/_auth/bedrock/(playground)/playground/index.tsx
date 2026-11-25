import { createFileRoute } from "@tanstack/react-router"

import { pageTitle } from "@/lib/branding"
import {
  foundationModelsQueryOptions,
  guardrailsQueryOptions,
} from "@/queries/bedrock"

import { PlaygroundPage } from "../-components/playground-page"

export const Route = createFileRoute("/_auth/bedrock/(playground)/playground/")(
  {
    loader: async ({ context }) => {
      await Promise.all([
        context.queryClient.query({
          ...foundationModelsQueryOptions,
          staleTime: "static",
        }),
        context.queryClient.query({
          ...guardrailsQueryOptions,
          staleTime: "static",
        }),
      ])
    },
    head: () => ({
      meta: [{ title: pageTitle("Playground | Ochre") }],
    }),
    component: PlaygroundPage,
  },
)
