import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { iamPoliciesQueryOptions } from "@/queries/iam"

import { PoliciesPage } from "../-components/policies-page"

export const Route = createFileRoute("/_auth/iam/(policies)/list-policies/")({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamPoliciesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Policies | IAM | Mulga" }],
  }),
  component: Policies,
})

function Policies() {
  const { data } = useSuspenseQuery(iamPoliciesQueryOptions)
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
    <PoliciesPage
      policies={data.Policies ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
