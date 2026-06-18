import { createFileRoute } from "@tanstack/react-router"

import { ecrRepositoriesQueryOptions } from "@/queries/ecr"

import { RepositoriesPage } from "../-components/repositories-page"

export const Route = createFileRoute(
  "/_auth/ecr/(repositories)/list-repositories/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ecrRepositoriesQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Repositories | ECR | Mulga",
      },
    ],
  }),
  component: RepositoriesPage,
})
