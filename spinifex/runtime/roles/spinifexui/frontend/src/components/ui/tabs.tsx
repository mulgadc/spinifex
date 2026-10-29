import { Tabs as TabsPrimitive } from "@base-ui/react/tabs"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

export function Tabs({ className, ...props }: TabsPrimitive.Root.Props) {
  return (
    <TabsPrimitive.Root
      className={cn("flex flex-col gap-4", className)}
      data-slot="tabs"
      {...props}
    />
  )
}

const tabsListVariants = cva("items-center border-b", {
  variants: {
    variant: {
      default: "inline-flex gap-1",
      // A detail panel's strip: icons only, centred across the panel.
      icon: "flex justify-center gap-2 px-4",
    },
  },
  defaultVariants: {
    variant: "default",
  },
})

export function TabsList({
  className,
  variant = "default",
  ...props
}: TabsPrimitive.List.Props & VariantProps<typeof tabsListVariants>) {
  return (
    <TabsPrimitive.List
      className={cn(tabsListVariants({ variant, className }))}
      data-slot="tabs-list"
      {...props}
    />
  )
}

const tabsTabVariants = cva(
  "inline-flex items-center justify-center border-b-2 border-transparent text-muted-foreground transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30 focus-visible:outline-none disabled:pointer-events-none disabled:opacity-50 data-active:border-primary data-active:text-foreground",
  {
    variants: {
      variant: {
        default: "px-3 py-2 text-sm font-medium",
        icon: "size-10 data-active:text-primary [&_svg]:size-4",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  },
)

export function TabsTab({
  className,
  variant = "default",
  ...props
}: TabsPrimitive.Tab.Props & VariantProps<typeof tabsTabVariants>) {
  return (
    <TabsPrimitive.Tab
      className={cn(tabsTabVariants({ variant, className }))}
      data-slot="tabs-tab"
      {...props}
    />
  )
}

export function TabsPanel({ className, ...props }: TabsPrimitive.Panel.Props) {
  return (
    <TabsPrimitive.Panel
      className={cn("focus-visible:outline-none", className)}
      data-slot="tabs-panel"
      {...props}
    />
  )
}
