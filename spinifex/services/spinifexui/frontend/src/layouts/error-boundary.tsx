import { useQueryClient } from "@tanstack/react-query"
import type { ErrorComponentProps } from "@tanstack/react-router"
import { Link, useRouter } from "@tanstack/react-router"
import { AlertCircle } from "lucide-react"

import { Header } from "@/components/header"
import { Button } from "@/components/ui/button"
import { SidebarLayout } from "@/layouts/sidebar-layout"

export function ErrorBoundary({ error, reset }: ErrorComponentProps) {
  const router = useRouter()
  const queryClient = useQueryClient()

  function handleTryAgain() {
    void queryClient.invalidateQueries()
    reset()
    void router.invalidate()
  }

  return (
    <>
      <SidebarLayout />
      <main className="flex flex-1 flex-col">
        <Header />
        <div className="flex flex-1 items-center justify-center p-6 pt-0">
          <div className="max-w-md space-y-4 text-center">
            <div className="flex justify-center">
              <AlertCircle className="size-12 text-destructive" />
            </div>
            <h1 className="text-2xl font-semibold">Something went wrong</h1>
            <p className="text-muted-foreground">
              {error.message || "An unexpected error occurred"}
            </p>
            <div className="flex justify-center gap-2">
              <Button onClick={handleTryAgain} variant="default">
                Try Again
              </Button>
              <Link to="/">
                <Button variant="outline">Go Home</Button>
              </Link>
            </div>
          </div>
        </div>
      </main>
    </>
  )
}
