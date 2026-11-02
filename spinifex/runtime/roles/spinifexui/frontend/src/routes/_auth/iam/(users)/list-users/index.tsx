import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { iamUsersQueryOptions } from "@/queries/iam"

import { UsersPage } from "../-components/users-page"

export const Route = createFileRoute("/_auth/iam/(users)/list-users/")({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamUsersQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Users | IAM | Mulga" }],
  }),
  component: Users,
})

function Users() {
  const { data } = useSuspenseQuery(iamUsersQueryOptions)
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
    <UsersPage
      onSearchChange={onSearchChange}
      search={search}
      users={data.Users ?? []}
    />
  )
}
