import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ensureTrailingSlash } from "@/lib/utils"
import { s3BucketObjectsQueryOptions } from "@/queries/s3"
import { ObjectBrowser } from "@/routes/_auth/s3/-components/object-browser"

export const Route = createFileRoute("/_auth/s3/ls/$bucket/$")({
  validateSearch: z.object(panelSearchFields),
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...s3BucketObjectsQueryOptions(
        params.bucket,
        ensureTrailingSlash(params._splat ?? ""),
      ),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.bucket}/${ensureTrailingSlash(params._splat ?? "")} | Object Storage | Mulga`,
      },
    ],
  }),
  component: FolderObjects,
})

function FolderObjects() {
  const { bucket, _splat } = Route.useParams()
  const folder = ensureTrailingSlash(_splat ?? "")
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next),
    })
  }

  // Keyed by folder, so entering another one starts its find and pages afresh.
  return (
    <ObjectBrowser
      bucket={bucket}
      folder={folder}
      key={`${bucket}/${folder}`}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
