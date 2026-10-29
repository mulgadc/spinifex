import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ec2LaunchTemplatesQueryOptions } from "@/queries/ec2"

import { LaunchTemplatesPage } from "../-components/launch-templates-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(launch-templates)/describe-launch-templates/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2LaunchTemplatesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Launch Templates | EC2 | Mulga",
      },
    ],
  }),
  component: LaunchTemplates,
})

function LaunchTemplates() {
  const { data } = useSuspenseQuery(ec2LaunchTemplatesQueryOptions)
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
    <LaunchTemplatesPage
      launchTemplates={data.LaunchTemplates ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
