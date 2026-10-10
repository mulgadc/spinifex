import { createFileRoute, notFound, Outlet } from "@tanstack/react-router"

import { isOchreEnabled } from "@/lib/cluster-config"

// A direct URL must 404 when the flag is off, not just be missing from the
// nav — otherwise the console is reachable by anyone who knows the path.
export const Route = createFileRoute("/_auth/bedrock")({
  beforeLoad: () => {
    if (!isOchreEnabled()) {
      notFound({ throw: true })
    }
  },
  component: () => <Outlet />,
})
