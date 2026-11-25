import { useNavigate } from "@tanstack/react-router"
import { Moon, Plus, Search, type LucideIcon } from "lucide-react"
import { useEffect, useState } from "react"

import { useTheme } from "@/components/theme-provider"
import { Button } from "@/components/ui/button"
import {
  Command,
  CommandCollection,
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandShortcut,
} from "@/components/ui/command"
import { useAdmin } from "@/hooks/use-admin"
import { isOchreEnabled } from "@/lib/cluster-config"
import { visibleNav } from "@/lib/nav"
import { IS_MAC } from "@/lib/shortcuts"

export const PALETTE_SHORTCUT = IS_MAC ? "⌘K" : "Ctrl K"

// Opens on ⌘K or Ctrl+K from anywhere, including inside inputs, as the
// shortcut is unclaimed by browsers on every platform the console supports.
function usePaletteShortcut(toggle: () => void) {
  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if (event.key.toLowerCase() === "k" && (event.metaKey || event.ctrlKey)) {
        event.preventDefault()
        toggle()
      }
    }

    document.addEventListener("keydown", onKeyDown)

    return () => {
      document.removeEventListener("keydown", onKeyDown)
    }
  }, [toggle])
}

// A group or AWS service typed in full ("compute", "ec2") lists first.
function namedFirst<T extends { label: string; service?: string }>(
  groups: readonly T[],
  search: string,
): T[] {
  const term = search.trim().toLowerCase()

  const named = (group: T) =>
    [group.label, group.service].some((name) => name?.toLowerCase() === term)

  return [...groups.filter(named), ...groups.filter((group) => !named(group))]
}

interface PaletteItem {
  label: string
  keywords: string[]
  icon: LucideIcon
  to?: string
  group?: string
}

interface PaletteGroup {
  label: string
  items: PaletteItem[]
}

function matchesCommand(item: PaletteItem, query: string) {
  const term = query.trim().toLowerCase()

  return [item.label, ...item.keywords].some((text) =>
    text.toLowerCase().includes(term),
  )
}

export function CommandPalette() {
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState("")

  function changeOpen(nextOpen: boolean) {
    setOpen(nextOpen)

    if (!nextOpen) {
      setSearch("")
    }
  }

  const navigate = useNavigate()
  const { isAdmin } = useAdmin()
  const { theme, setTheme } = useTheme()

  const groups = visibleNav({ isAdmin, ochreEnabled: isOchreEnabled() })

  const creates = groups.flatMap((group) =>
    group.items.flatMap((item) =>
      item.create ? [{ group: group.label, ...item.create }] : [],
    ),
  )

  const commandGroups: PaletteGroup[] = [
    ...namedFirst(groups, search).map((group) => ({
      label: group.label,
      items: group.items.map((item) => ({
        label: item.label,
        keywords: [group.label, group.service ?? ""],
        icon: item.icon,
        to: item.to,
      })),
    })),
    {
      label: "Create",
      items: creates.map((create) => ({
        label: create.label,
        keywords: [create.group, "new", "create"],
        icon: Plus,
        to: create.to,
        group: create.group,
      })),
    },
    {
      label: "Preferences",
      items: [
        {
          label: "Toggle theme",
          keywords: ["Preferences", "dark", "light", "appearance"],
          icon: Moon,
        },
      ],
    },
  ]

  usePaletteShortcut(() => {
    changeOpen(!open)
  })

  function select(item: PaletteItem) {
    changeOpen(false)

    if (item.to) {
      void navigate({ to: item.to })
    } else {
      setTheme(theme === "light" ? "dark" : "light")
    }
  }

  return (
    <>
      <Button
        aria-keyshortcuts={IS_MAC ? "Meta+K" : "Control+K"}
        onClick={() => {
          setOpen(true)
        }}
        size="sm"
        variant="outline"
      >
        <Search data-icon="inline-start" />
        <span className="hidden sm:inline">Search</span>
        <kbd className="hidden font-mono text-2xs text-muted-foreground sm:inline">
          {PALETTE_SHORTCUT}
        </kbd>
      </Button>
      <CommandDialog
        description="Jump to a service or start a create flow."
        onOpenChange={changeOpen}
        open={open}
        title="Command palette"
      >
        <Command<PaletteItem>
          filter={matchesCommand}
          items={commandGroups}
          onValueChange={setSearch}
          value={search}
        >
          <CommandInput
            aria-label="Search commands"
            placeholder="Go to a service or run a command"
          />
          <CommandEmpty>No matches.</CommandEmpty>
          <CommandList>
            {(group: PaletteGroup) => (
              <CommandGroup
                heading={group.label}
                items={group.items}
                key={group.label}
              >
                <CommandCollection>
                  {(item: PaletteItem) => (
                    <CommandItem
                      key={item.to ?? item.label}
                      onClick={() => {
                        select(item)
                      }}
                      value={item}
                    >
                      <item.icon />
                      {item.label}
                      {item.group && (
                        <CommandShortcut>{item.group}</CommandShortcut>
                      )}
                    </CommandItem>
                  )}
                </CommandCollection>
              </CommandGroup>
            )}
          </CommandList>
        </Command>
      </CommandDialog>
    </>
  )
}
