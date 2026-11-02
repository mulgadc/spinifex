import { createFileRoute, Outlet, redirect } from "@tanstack/react-router"

import { UploadProgressDialog } from "@/components/upload-progress-dialog"
import { getCredentials } from "@/lib/auth"

export const Route = createFileRoute("/_auth")({
  beforeLoad: () => {
    if (!getCredentials()) {
      throw redirect({ to: "/login" })
    }
  },
  component: SignedIn,
})

// Uploads outlive the page that started them, so their dialog lives here.
function SignedIn() {
  return (
    <>
      <Outlet />
      <UploadProgressDialog />
    </>
  )
}
