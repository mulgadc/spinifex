import type { QueryClient } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"

import { requireAdmin } from "@/lib/require-admin"

import { DesignCataloguePage } from "./-components/design/design-catalogue-page"

// The catalogue is a reference for building pages, so it is open in dev
// builds and admin-only in a release.
async function requireDevOrAdmin(options: {
  context: { queryClient: QueryClient }
}): Promise<void> {
  if (import.meta.env.DEV) {
    return
  }

  await requireAdmin(options)
}

export const Route = createFileRoute("/_auth/design")({
  beforeLoad: requireDevOrAdmin,
  head: () => ({
    meta: [{ title: "Design system | Mulga" }],
  }),
  component: DesignCataloguePage,
})
