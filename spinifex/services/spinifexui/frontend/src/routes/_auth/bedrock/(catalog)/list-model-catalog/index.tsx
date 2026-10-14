import { createFileRoute } from "@tanstack/react-router"

import { RequireAdmin } from "@/components/require-admin"

import { ModelCatalogPage } from "../-components/model-catalog-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(catalog)/list-model-catalog/",
)({
  head: () => ({
    meta: [{ title: "Model Catalog | Ochre | Mulga" }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  return (
    <RequireAdmin>
      <ModelCatalogPage />
    </RequireAdmin>
  )
}
