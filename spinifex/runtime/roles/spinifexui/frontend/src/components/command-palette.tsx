import { useNavigate } from "@tanstack/react-router"
import { Moon, Plus, Search } from "lucide-react"
import { useEffect, useState } from "react"

import { useTheme } from "@/components/theme-provider"
import { Button } from "@/components/ui/button"
import {
  Command,
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

export function CommandPalette() {
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState("")

  // However the palette closes, it opens empty next time.
  if (!open && search !== "") {
    setSearch("")
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

  usePaletteShortcut(() => {
    setOpen((current) => !current)
  })

  function go(to: string) {
    setOpen(false)
    void navigate({ to })
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
        onOpenChange={setOpen}
        open={open}
        title="Command palette"
      >
        <Command>
          <CommandInput
            onValueChange={setSearch}
            placeholder="Go to a service or run a command"
            value={search}
          />
          <CommandList>
            <CommandEmpty>No matches.</CommandEmpty>
            {namedFirst(groups, search).map((group) => (
              <CommandGroup
                heading={group.label}
                key={group.label}
                value={group.label}
              >
                {group.items.map((item) => (
                  <CommandItem
                    keywords={[group.label, group.service ?? ""]}
                    key={item.to}
                    onSelect={() => {
                      go(item.to)
                    }}
                    value={`${group.label} ${item.label}`}
                  >
                    <item.icon />
                    {item.label}
                  </CommandItem>
                ))}
              </CommandGroup>
            ))}
            <CommandGroup heading="Create">
              {creates.map((create) => (
                <CommandItem
                  keywords={[create.group, "new", "create"]}
                  key={create.to}
                  onSelect={() => {
                    go(create.to)
                  }}
                  value={create.label}
                >
                  <Plus />
                  {create.label}
                  <CommandShortcut>{create.group}</CommandShortcut>
                </CommandItem>
              ))}
            </CommandGroup>
            <CommandGroup heading="Preferences">
              <CommandItem
                keywords={["dark", "light", "appearance"]}
                onSelect={() => {
                  setTheme(theme === "light" ? "dark" : "light")
                  setOpen(false)
                }}
                value="Toggle theme"
              >
                <Moon />
                Toggle theme
              </CommandItem>
            </CommandGroup>
          </CommandList>
        </Command>
      </CommandDialog>
    </>
  )
}
