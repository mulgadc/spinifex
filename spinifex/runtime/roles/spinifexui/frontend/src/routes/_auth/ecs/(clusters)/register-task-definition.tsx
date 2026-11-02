import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the task definitions list's side panel; this address is
// kept for links.
export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/register-task-definition",
)({
  beforeLoad: () => {
    throw redirect({ to: "/ecs/task-definitions", search: { create: true } })
  },
})
