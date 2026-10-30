import type { ReactNode } from "react"

import { cn } from "@/lib/utils"

interface FormLockProps {
  locked: boolean
  className?: string
  children: ReactNode
}

// Disables every control inside while a form is submitting, and lets go when
// the request settles, so an error leaves the entries ready to correct.
export function FormLock({ locked, className, children }: FormLockProps) {
  return (
    <fieldset className={cn("min-w-0", className)} disabled={locked}>
      {children}
    </fieldset>
  )
}
