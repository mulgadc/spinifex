import type { ComponentProps } from "react"

import { cn } from "@/lib/utils"

// Buttons joined into one control, as a split button or a segmented set: the
// inner corners square off and neighbouring borders overlap.
export function ButtonGroup({
  className,
  ...props
}: ComponentProps<"fieldset">) {
  return (
    <fieldset
      className={cn(
        "flex w-fit min-w-0 items-stretch *:not-first:-ml-px *:not-first:rounded-l-none *:not-last:rounded-r-none *:focus-visible:z-10",
        className,
      )}
      data-slot="button-group"
      {...props}
    />
  )
}
