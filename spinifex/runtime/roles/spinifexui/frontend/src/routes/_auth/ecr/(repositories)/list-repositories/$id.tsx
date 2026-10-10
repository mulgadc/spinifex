import { createFileRoute } from "@tanstack/react-router"

import { safeDecodeURIComponent } from "@/lib/utils"
import {
  ecrRepositoriesQueryOptions,
  ecrRepositoryImagesQueryOptions,
  ecrRepositoryPolicyQueryOptions,
} from "@/queries/ecr"

import { RepositoryDetailPage } from "../-components/repository-detail-page"

export const Route = createFileRoute(
  "/_auth/ecr/(repositories)/list-repositories/$id",
)({
  loader: async ({ context, params }) => {
    const name = safeDecodeURIComponent(params.id)
    await Promise.all([
      context.queryClient.query({
        ...ecrRepositoriesQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ecrRepositoryImagesQueryOptions(name),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ecrRepositoryPolicyQueryOptions(name),
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${safeDecodeURIComponent(params.id)} | Repository | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { id } = Route.useParams()
  return <RepositoryDetailPage repositoryName={safeDecodeURIComponent(id)} />
}
