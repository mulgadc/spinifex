import { createFileRoute } from "@tanstack/react-router"

import { pageTitle } from "@/lib/branding"
import { requireAdmin } from "@/lib/require-admin"

import { ModelCatalogPage } from "../-components/model-catalog-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(catalog)/list-model-catalog/",
)({
  beforeLoad: requireAdmin,
  head: () => ({
    meta: [{ title: pageTitle("Model Catalog | Ochre") }],
  }),
  component: ModelCatalogPage,
})
