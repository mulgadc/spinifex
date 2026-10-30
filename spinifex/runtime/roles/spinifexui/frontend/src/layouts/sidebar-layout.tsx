import { useQueryClient } from "@tanstack/react-query"
import { Link, useLocation, useNavigate } from "@tanstack/react-router"
import { BookOpen, ChevronRight, LogOut } from "lucide-react"
import { useState } from "react"

import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
  SidebarSeparator,
  useSidebar,
} from "@/components/ui/sidebar"
import { useAdmin } from "@/hooks/use-admin"
import { isOchreEnabled } from "@/lib/cluster-config"
import { activeNavItem, type NavGroup, visibleNav } from "@/lib/nav"
import { signOut } from "@/lib/query-client"

const COLLAPSED_GROUPS_KEY = "spinifex-ui-nav-collapsed"

function readCollapsedGroups(): string[] {
  try {
    const parsed: unknown = JSON.parse(
      localStorage.getItem(COLLAPSED_GROUPS_KEY) ?? "[]",
    )

    return Array.isArray(parsed)
      ? parsed.filter((label): label is string => typeof label === "string")
      : []
  } catch {
    return []
  }
}

// A group the user folded stays folded across reloads. Navigating into a group
// unfolds it so the active item is visible, and it can then be folded again.
function useCollapsedGroups(activeLabel: string | undefined) {
  const [collapsed, setCollapsed] = useState(readCollapsedGroups)
  const [shownActive, setShownActive] = useState<string>()

  if (activeLabel !== shownActive) {
    setShownActive(activeLabel)

    if (activeLabel && collapsed.includes(activeLabel)) {
      setCollapsed(collapsed.filter((other) => other !== activeLabel))
    }
  }

  function setGroupOpen(label: string, open: boolean) {
    const next = open
      ? collapsed.filter((other) => other !== label)
      : [...new Set([...collapsed, label])]

    setCollapsed(next)

    try {
      localStorage.setItem(COLLAPSED_GROUPS_KEY, JSON.stringify(next))
    } catch {
      // Storage disabled: the fold still holds for this session.
    }
  }

  return { collapsed, setGroupOpen }
}

function NavSection({
  group,
  activeTo,
  open,
  onOpenChange,
  onNavigate,
}: {
  group: NavGroup
  activeTo: string | undefined
  open: boolean
  onOpenChange: (open: boolean) => void
  onNavigate: () => void
}) {
  return (
    <Collapsible onOpenChange={onOpenChange} open={open}>
      <SidebarGroup>
        <SidebarGroupLabel
          render={<CollapsibleTrigger className="group/nav-trigger w-full" />}
        >
          <span className="flex-1 text-left">{group.label}</span>
          <ChevronRight className="transition-transform group-data-panel-open/nav-trigger:rotate-90 motion-reduce:transition-none" />
        </SidebarGroupLabel>
        <CollapsibleContent>
          <SidebarMenu>
            {group.items.map((item) => (
              <SidebarMenuItem key={item.to}>
                <SidebarMenuButton
                  isActive={item.to === activeTo}
                  onClick={onNavigate}
                  render={<Link to={item.to} />}
                  tooltip={item.tooltip ?? item.label}
                >
                  <item.icon className="size-4" />
                  <span>{item.label}</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            ))}
          </SidebarMenu>
        </CollapsibleContent>
      </SidebarGroup>
    </Collapsible>
  )
}

export function SidebarLayout() {
  const pathname = useLocation({
    select: (location) => location.pathname,
  })

  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { isAdmin } = useAdmin()
  const groups = visibleNav({ isAdmin, ochreEnabled: isOchreEnabled() })
  const active = activeNavItem(groups, pathname)
  const { collapsed, setGroupOpen } = useCollapsedGroups(active?.group.label)
  const { state, setOpenMobile } = useSidebar()
  // Icon-only mode has no group labels to unfold, so every item shows.
  const iconOnly = state === "collapsed"

  function handleLogout() {
    signOut(queryClient)
    navigate({ to: "/login" })
  }

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader variant="brand">
        <img
          src="/mulga-logo.svg"
          alt="Mulga"
          className="size-6 shrink-0 dark:invert"
        />
        <span className="truncate text-sm font-semibold group-data-[collapsible=icon]:hidden">
          Spinifex
        </span>
      </SidebarHeader>
      <SidebarContent>
        {groups.map((group) => (
          <NavSection
            activeTo={active?.item.to}
            group={group}
            key={group.label}
            // The phone sheet covers the page, so following a link closes it.
            onNavigate={() => {
              setOpenMobile(false)
            }}
            onOpenChange={(open) => {
              setGroupOpen(group.label, open)
            }}
            open={iconOnly || !collapsed.includes(group.label)}
          />
        ))}
        {isAdmin && (
          <SidebarGroup>
            <SidebarGroupLabel>Documentation</SidebarGroupLabel>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton
                  render={
                    // oxlint-disable-next-line jsx-a11y/control-has-associated-label -- SidebarMenuButton supplies the label
                    <a
                      href="https://docs.mulgadc.com"
                      rel="noopener noreferrer"
                      target="_blank"
                    />
                  }
                  tooltip="Documentation"
                >
                  <BookOpen className="size-4" />
                  <span>Docs</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarGroup>
        )}
      </SidebarContent>

      <SidebarFooter>
        <SidebarSeparator className="mx-0 w-full" />
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton onClick={handleLogout} tooltip="Sign out">
              <LogOut className="size-4" />
              <span>Sign out</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>

      <SidebarRail />
    </Sidebar>
  )
}
