import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { iamRolesQueryOptions } from "@/queries/iam"

import { RolesPage } from "../-components/roles-page"

export const Route = createFileRoute("/_auth/iam/(roles)/list-roles/")({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamRolesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Roles | IAM | Mulga" }],
  }),
  component: Roles,
})

function Roles() {
  const { data } = useSuspenseQuery(iamRolesQueryOptions)
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
    <RolesPage
      roles={data.Roles ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
