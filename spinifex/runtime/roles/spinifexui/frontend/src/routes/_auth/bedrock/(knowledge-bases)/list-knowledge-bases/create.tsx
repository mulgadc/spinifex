import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/bedrock/(knowledge-bases)/list-knowledge-bases/create",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/bedrock/list-knowledge-bases",
      search: { create: true },
    })
  },
})
