import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { iamInstanceProfilesQueryOptions } from "@/queries/iam"

import { InstanceProfilesPage } from "../-components/instance-profiles-page"

export const Route = createFileRoute(
  "/_auth/iam/(instance-profiles)/list-instance-profiles/",
)({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamInstanceProfilesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Instance profiles | IAM | Mulga" }],
  }),
  component: InstanceProfiles,
})

function InstanceProfiles() {
  const { data } = useSuspenseQuery(iamInstanceProfilesQueryOptions)
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
    <InstanceProfilesPage
      instanceProfiles={data.InstanceProfiles ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
