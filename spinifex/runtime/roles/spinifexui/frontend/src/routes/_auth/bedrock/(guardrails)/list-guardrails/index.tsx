import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { guardrailsQueryOptions } from "@/queries/bedrock"

import { GuardrailsPage } from "../-components/guardrails-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(guardrails)/list-guardrails/",
)({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...guardrailsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Guardrails | Ochre | Mulga" }],
  }),
  component: Guardrails,
})

function Guardrails() {
  const { data } = useSuspenseQuery(guardrailsQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return (
    <GuardrailsPage
      guardrails={data.guardrails ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
