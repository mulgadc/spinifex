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
      context.queryClient.ensureQueryData(ecrRepositoriesQueryOptions),
      context.queryClient.ensureQueryData(
        ecrRepositoryImagesQueryOptions(name),
      ),
      context.queryClient.ensureQueryData(
        ecrRepositoryPolicyQueryOptions(name),
      ),
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
