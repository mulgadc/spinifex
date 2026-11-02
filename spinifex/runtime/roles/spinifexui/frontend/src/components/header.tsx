import { AppBreadcrumbs } from "@/components/app-breadcrumbs"
import { CommandPalette } from "@/components/command-palette"
import { ThemeSwitch } from "@/components/theme-switch"
import { buttonVariants } from "@/components/ui/button"
import { SidebarTrigger } from "@/components/ui/sidebar"
import { useAdmin } from "@/hooks/use-admin"
import { cn } from "@/lib/utils"

export function Header() {
  const { isAdmin, license } = useAdmin()
  const showUpgrade = isAdmin && license === "open-source"

  return (
    <header
      className={cn(
        "z-50 flex h-12 shrink-0 items-center gap-3 border-b border-sidebar-border bg-sidebar px-4 text-sidebar-foreground",
        "sticky top-0",
      )}
    >
      <SidebarTrigger className="-ml-1" />
      <AppBreadcrumbs />
      <div className="ml-auto flex shrink-0 items-center gap-2">
        <CommandPalette />
        {showUpgrade && (
          <a
            href="https://mulgadc.com/purchase"
            target="_blank"
            rel="noopener noreferrer"
            className={cn(buttonVariants({ variant: "outline", size: "sm" }))}
          >
            Upgrade
          </a>
        )}
        <ThemeSwitch />
      </div>
    </header>
  )
}
