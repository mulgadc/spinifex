import { createFileRoute, redirect } from "@tanstack/react-router"

import { useAdmin } from "@/contexts/admin-context"

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
  const { isAdmin } = useAdmin()

  if (!isAdmin) {
    throw redirect({ to: "/" })
  }

  return <ModelCatalogPage />
}
