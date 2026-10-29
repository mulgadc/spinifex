import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ec2VolumesQueryOptions } from "@/queries/ec2"

import { VolumesPage } from "../-components/volumes-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute("/_auth/ec2/(volumes)/describe-volumes/")({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2VolumesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Volumes | EC2 | Mulga",
      },
    ],
  }),
  component: Volumes,
})

function Volumes() {
  const { data } = useSuspenseQuery(ec2VolumesQueryOptions)
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
    <VolumesPage
      onSearchChange={onSearchChange}
      search={search}
      volumes={data.Volumes ?? []}
    />
  )
}
