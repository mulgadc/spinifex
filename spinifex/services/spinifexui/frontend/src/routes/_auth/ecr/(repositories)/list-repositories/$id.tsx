import { createFileRoute } from "@tanstack/react-router"

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
    const name = decodeURIComponent(params.id)
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
        title: `${decodeURIComponent(params.id)} | Repository | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { id } = Route.useParams()
  return <RepositoryDetailPage repositoryName={decodeURIComponent(id)} />
}
