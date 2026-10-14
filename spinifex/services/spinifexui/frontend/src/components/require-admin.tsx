import { Navigate } from "@tanstack/react-router"

import { useAdmin } from "@/contexts/admin-context"

// Renders nothing until admin detection settles, so a cold load does not
// bounce an admin away before GetCallerIdentity returns.
export function RequireAdmin({
  children,
}: {
  children: React.ReactNode
}): React.ReactNode {
  const { isAdmin, loading } = useAdmin()

  if (loading) {
    return null
  }
  if (!isAdmin) {
    return <Navigate replace to="/" />
  }
  return children
}
