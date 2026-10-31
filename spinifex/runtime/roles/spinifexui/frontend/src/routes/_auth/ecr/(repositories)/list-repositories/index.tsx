import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ecrRepositoriesQueryOptions } from "@/queries/ecr"

import { RepositoriesPage } from "../-components/repositories-page"

export const Route = createFileRoute(
  "/_auth/ecr/(repositories)/list-repositories/",
)({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ecrRepositoriesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Repositories | ECR | Mulga",
      },
    ],
  }),
  component: Repositories,
})

function Repositories() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return <RepositoriesPage onSearchChange={onSearchChange} search={search} />
}
