import { Dialog as SheetPrimitive } from "@base-ui/react/dialog"
import { XIcon } from "lucide-react"
import type * as React from "react"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

export function Sheet({ ...props }: SheetPrimitive.Root.Props) {
  return <SheetPrimitive.Root data-slot="sheet" {...props} />
}

export function SheetTrigger({ ...props }: SheetPrimitive.Trigger.Props) {
  return <SheetPrimitive.Trigger data-slot="sheet-trigger" {...props} />
}

export function SheetClose({ ...props }: SheetPrimitive.Close.Props) {
  return <SheetPrimitive.Close data-slot="sheet-close" {...props} />
}

export function SheetPortal({ ...props }: SheetPrimitive.Portal.Props) {
  return <SheetPrimitive.Portal data-slot="sheet-portal" {...props} />
}

export function SheetOverlay({
  className,
  ...props
}: SheetPrimitive.Backdrop.Props) {
  return (
    <SheetPrimitive.Backdrop
      className={cn(
        "data-closed:fade-out-0 data-open:fade-in-0 data-closed:animate-out data-open:animate-in fixed inset-0 z-50 bg-black/80 duration-100 data-ending-style:opacity-0 data-starting-style:opacity-0 supports-backdrop-filter:backdrop-blur-xs",
        className,
      )}
      data-slot="sheet-overlay"
      {...props}
    />
  )
}

export function SheetContent({
  className,
  children,
  side = "right",
  showCloseButton = true,
  ...props
}: SheetPrimitive.Popup.Props & {
  side?: "top" | "right" | "bottom" | "left"
  showCloseButton?: boolean
}) {
  return (
    <SheetPortal>
      <SheetOverlay />
      <SheetPrimitive.Popup
        className={cn(
          "data-[side=right]:data-closed:slide-out-to-right-10 data-[side=right]:data-open:slide-in-from-right-10 data-[side=left]:data-closed:slide-out-to-left-10 data-[side=left]:data-open:slide-in-from-left-10 data-[side=top]:data-closed:slide-out-to-top-10 data-[side=top]:data-open:slide-in-from-top-10 data-closed:fade-out-0 data-open:fade-in-0 data-[side=bottom]:data-closed:slide-out-to-bottom-10 data-[side=bottom]:data-open:slide-in-from-bottom-10 data-closed:animate-out data-open:animate-in fixed z-50 flex flex-col bg-background bg-clip-padding text-xs/relaxed shadow-lg transition duration-200 ease-in-out data-[side=bottom]:inset-x-0 data-[side=bottom]:bottom-0 data-[side=bottom]:h-auto data-[side=bottom]:border-t data-[side=left]:inset-y-0 data-[side=left]:left-0 data-[side=left]:h-full data-[side=left]:w-3/4 data-[side=left]:border-r data-[side=right]:inset-y-0 data-[side=right]:right-0 data-[side=right]:h-full data-[side=right]:w-3/4 data-[side=right]:border-l data-[side=top]:inset-x-0 data-[side=top]:top-0 data-[side=top]:h-auto data-[side=top]:border-b data-[side=left]:sm:max-w-sm data-[side=right]:sm:max-w-sm",
          className,
        )}
        data-side={side}
        data-slot="sheet-content"
        {...props}
      >
        {children}
        {showCloseButton && (
          <SheetPrimitive.Close
            data-slot="sheet-close"
            render={
              <Button
                className="absolute top-4 right-4"
                size="icon-sm"
                variant="ghost"
              />
            }
          >
            <XIcon />
            <span className="sr-only">Close</span>
          </SheetPrimitive.Close>
        )}
      </SheetPrimitive.Popup>
    </SheetPortal>
  )
}

export function SheetHeader({
  className,
  ...props
}: React.ComponentProps<"div">) {
  return (
    <div
      className={cn("flex flex-col gap-1.5 p-6", className)}
      data-slot="sheet-header"
      {...props}
    />
  )
}

export function SheetFooter({
  className,
  ...props
}: React.ComponentProps<"div">) {
  return (
    <div
      className={cn("mt-auto flex flex-col gap-2 p-6", className)}
      data-slot="sheet-footer"
      {...props}
    />
  )
}

export function SheetTitle({
  className,
  ...props
}: SheetPrimitive.Title.Props) {
  return (
    <SheetPrimitive.Title
      className={cn("text-sm font-medium text-foreground", className)}
      data-slot="sheet-title"
      {...props}
    />
  )
}

export function SheetDescription({
  className,
  ...props
}: SheetPrimitive.Description.Props) {
  return (
    <SheetPrimitive.Description
      className={cn("text-xs/relaxed text-muted-foreground", className)}
      data-slot="sheet-description"
      {...props}
    />
  )
}
