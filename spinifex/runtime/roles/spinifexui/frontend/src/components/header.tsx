import { ThemeSwitch } from "@/components/theme-switch"
import { buttonVariants } from "@/components/ui/button"
import { SidebarTrigger } from "@/components/ui/sidebar"
import { useAdmin } from "@/contexts/admin-context"
import { cn } from "@/lib/utils"

export function Header() {
  const { isAdmin, license } = useAdmin()
  const showUpgrade = isAdmin && license === "open-source"

  return (
    <header
      className={cn(
        "z-50 flex h-10 shrink-0 items-center gap-2 border-b border-sidebar-border bg-sidebar px-4 text-sidebar-foreground",
        "sticky top-0",
      )}
    >
      <SidebarTrigger className="-ml-1" />
      <div className="flex w-full items-center justify-end gap-2">
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
