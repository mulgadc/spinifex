import { createFileRoute, redirect } from "@tanstack/react-router"

import { useAdmin } from "@/contexts/admin-context"

import { ModelAccessPage } from "../-components/model-access-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(model-access)/list-model-access/",
)({
  head: () => ({
    meta: [{ title: "Model Access | Ochre | Mulga" }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { isAdmin } = useAdmin()

  if (!isAdmin) {
    throw redirect({ to: "/" })
  }

  return <ModelAccessPage />
}
