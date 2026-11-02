import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { s3BucketObjectsQueryOptions } from "@/queries/s3"
import { ObjectBrowser } from "@/routes/_auth/s3/-components/object-browser"

export const Route = createFileRoute("/_auth/s3/ls/$bucket/")({
  validateSearch: z.object(panelSearchFields),
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...s3BucketObjectsQueryOptions(params.bucket, ""),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.bucket} | Object Storage | Mulga`,
      },
    ],
  }),
  component: BucketObjects,
})

function BucketObjects() {
  const { bucket } = Route.useParams()
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next),
    })
  }

  return (
    <ObjectBrowser
      bucket={bucket}
      folder=""
      key={bucket}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
