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
