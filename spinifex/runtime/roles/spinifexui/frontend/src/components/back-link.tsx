import type { LinkProps } from "@tanstack/react-router"
import { Link } from "@tanstack/react-router"
import { ArrowLeft } from "lucide-react"

interface BackLinkProps extends Omit<LinkProps, "children"> {
  children: string
}

export function BackLink({ children, ...linkProps }: BackLinkProps) {
  return (
    <Link
      className="mb-6 inline-flex items-center gap-2 text-sm"
      {...linkProps}
    >
      <ArrowLeft className="size-4" />
      {children}
    </Link>
  )
}
