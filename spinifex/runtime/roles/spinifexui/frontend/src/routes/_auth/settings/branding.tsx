import { createFileRoute, redirect } from "@tanstack/react-router"

import { getBranding } from "@/lib/branding"
import { requireAdmin } from "@/lib/require-admin"

import { BrandingPage } from "./-components/branding-page"

export const Route = createFileRoute("/_auth/settings/branding")({
  beforeLoad: async (ctx) => {
    await requireAdmin(ctx)

    // With the gate off the page could only be refused, so it is not offered.
    if (!getBranding().enabled) {
      throw redirect({ to: "/" })
    }
  },
  head: () => ({
    meta: [{ title: "Branding | Settings" }],
  }),
  component: BrandingPage,
})
