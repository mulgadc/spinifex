import { Tabs as TabsPrimitive } from "@base-ui/react/tabs"

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

export function TabsList({ className, ...props }: TabsPrimitive.List.Props) {
  return (
    <TabsPrimitive.List
      className={cn("inline-flex items-center gap-1 border-b", className)}
      data-slot="tabs-list"
      {...props}
    />
  )
}

export function TabsTab({ className, ...props }: TabsPrimitive.Tab.Props) {
  return (
    <TabsPrimitive.Tab
      className={cn(
        "inline-flex items-center justify-center border-b-2 border-transparent px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30 focus-visible:outline-none disabled:pointer-events-none disabled:opacity-50 data-selected:border-primary data-selected:text-foreground",
        className,
      )}
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
