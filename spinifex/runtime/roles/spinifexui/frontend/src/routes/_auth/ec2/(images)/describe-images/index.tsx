import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { isSystemManagedImage } from "@/lib/system-managed"
import { ec2ImagesQueryOptions } from "@/queries/ec2"

import { ImagesPage } from "../-components/images-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute("/_auth/ec2/(images)/describe-images/")({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2ImagesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Images | EC2 | Mulga",
      },
    ],
  }),
  component: Images,
})

function Images() {
  const { data } = useSuspenseQuery(ec2ImagesQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  const images = (data.Images ?? []).filter(
    (image) => !isSystemManagedImage(image),
  )

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return (
    <ImagesPage
      images={images}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
