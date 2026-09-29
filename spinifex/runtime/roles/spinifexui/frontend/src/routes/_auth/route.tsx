import { createFileRoute, Outlet, redirect } from "@tanstack/react-router"

import { getCredentials } from "@/lib/auth"

export const Route = createFileRoute("/_auth")({
  beforeLoad: () => {
    if (!getCredentials()) {
      throw redirect({ to: "/login" })
    }
  },
  component: () => <Outlet />,
})
