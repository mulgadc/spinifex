import { Collapsible as CollapsiblePrimitive } from "@base-ui/react/collapsible"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

export function Collapsible({ ...props }: CollapsiblePrimitive.Root.Props) {
  return <CollapsiblePrimitive.Root data-slot="collapsible" {...props} />
}

export const collapsibleTriggerVariants = cva("", {
  variants: {
    variant: {
      default: "",
      field:
        "flex h-7 w-full items-center justify-between rounded-md border border-input bg-input/20 px-2 py-0.5 text-sm transition-colors outline-none hover:bg-input/40 focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30 md:text-xs/relaxed dark:bg-input/30 dark:hover:bg-input/50",
      link: "cursor-pointer text-xs text-muted-foreground underline-offset-4 hover:underline",
      subtle:
        "text-left text-sm font-medium text-muted-foreground hover:text-foreground",
      // A detail panel's section heading, which folds the section away.
      section:
        "group/section flex h-10 w-full items-center justify-between gap-2 px-4 text-left text-xs font-semibold tracking-wide text-muted-foreground uppercase outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30",
    },
  },
  defaultVariants: {
    variant: "default",
  },
})

export function CollapsibleTrigger({
  className,
  variant = "default",
  ...props
}: CollapsiblePrimitive.Trigger.Props &
  VariantProps<typeof collapsibleTriggerVariants>) {
  return (
    <CollapsiblePrimitive.Trigger
      className={cn(collapsibleTriggerVariants({ variant, className }))}
      data-slot="collapsible-trigger"
      {...props}
    />
  )
}

export const collapsibleContentVariants = cva("", {
  variants: {
    variant: {
      default: "",
      panel: "space-y-4 rounded-md border border-border p-4",
      indented: "space-y-4 border-l-2 border-muted pl-4",
      section: "px-4 pb-4",
      // A sidebar service group: grows open and folds shut. Clipped while it
      // moves, so the padding leaves its items' focus rings room.
      nav: "-m-0.5 h-(--collapsible-panel-height) overflow-hidden p-0.5 transition-all duration-200 ease-out data-ending-style:h-0 data-ending-style:opacity-0 data-ending-style:duration-150 data-ending-style:ease-in data-starting-style:h-0 data-starting-style:opacity-0 motion-reduce:transition-none",
    },
  },
  defaultVariants: {
    variant: "default",
  },
})

export function CollapsibleContent({
  className,
  variant = "default",
  ...props
}: CollapsiblePrimitive.Panel.Props &
  VariantProps<typeof collapsibleContentVariants>) {
  return (
    <CollapsiblePrimitive.Panel
      className={cn(collapsibleContentVariants({ variant, className }))}
      data-slot="collapsible-content"
      {...props}
    />
  )
}
