import { createFileRoute } from "@tanstack/react-router"

import { requireAdmin } from "@/lib/require-admin"

import { ModelAccessPage } from "../-components/model-access-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(model-access)/list-model-access/",
)({
  beforeLoad: requireAdmin,
  head: () => ({
    meta: [{ title: "Model Access | Ochre | Mulga" }],
  }),
  component: ModelAccessPage,
})
