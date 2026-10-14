import { createFileRoute } from "@tanstack/react-router"

import { RequireAdmin } from "@/components/require-admin"

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
  return (
    <RequireAdmin>
      <ModelAccessPage />
    </RequireAdmin>
  )
}
