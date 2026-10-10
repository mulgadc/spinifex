import { Link } from "@tanstack/react-router"

import { Button } from "@/components/ui/button"

export function NotFound() {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-4">
      <h1 className="text-4xl font-bold">404</h1>
      <p className="text-muted-foreground">Page not found</p>
      <Link to="/">
        <Button variant="outline">Go back home</Button>
      </Link>
    </div>
  )
}
