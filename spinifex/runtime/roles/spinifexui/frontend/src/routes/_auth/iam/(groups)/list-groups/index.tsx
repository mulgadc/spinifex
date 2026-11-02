import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { iamGroupsQueryOptions } from "@/queries/iam"

import { GroupsPage } from "../-components/groups-page"

export const Route = createFileRoute("/_auth/iam/(groups)/list-groups/")({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamGroupsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Groups | IAM | Mulga" }],
  }),
  component: Groups,
})

function Groups() {
  const { data } = useSuspenseQuery(iamGroupsQueryOptions)
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
    <GroupsPage
      groups={data.Groups ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
