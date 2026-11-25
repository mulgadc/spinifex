import { createFileRoute } from "@tanstack/react-router"

import { requireAdmin } from "@/lib/require-admin"

import { BrandingPage } from "./-components/branding-page"

export const Route = createFileRoute("/_auth/settings/branding")({
  beforeLoad: async (ctx) => {
    await requireAdmin(ctx)
  },
  head: () => ({
    meta: [{ title: "Branding | Settings" }],
  }),
  component: BrandingPage,
})
