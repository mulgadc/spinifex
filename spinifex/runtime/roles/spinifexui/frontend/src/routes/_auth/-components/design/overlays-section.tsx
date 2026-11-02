import { Button } from "@/components/ui/button"
import {
  Command,
  CommandCollection,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandShortcut,
} from "@/components/ui/command"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  HoverCard,
  HoverCardContent,
  HoverCardTrigger,
} from "@/components/ui/hover-card"
import {
  Popover,
  PopoverContent,
  PopoverDescription,
  PopoverHeader,
  PopoverTitle,
  PopoverTrigger,
} from "@/components/ui/popover"
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

import { CatalogueSection, Specimen } from "./catalogue-section"

const specimenCommands = [
  {
    label: "Go to",
    items: [
      { label: "Instances", shortcut: "G I" },
      { label: "VPCs", shortcut: "G V" },
    ],
  },
  {
    label: "Actions",
    items: [
      { label: "Launch instance", shortcut: "" },
      { label: "Create VPC", shortcut: "" },
    ],
  },
]

type SpecimenCommand = (typeof specimenCommands)[number]["items"][number]

export function OverlaysSection() {
  return (
    <CatalogueSection
      description="Overlays are the one place shadows are used."
      title="Overlays"
    >
      <Specimen label="Dialog, sheet, popover, hover card, tooltip, menu">
        <Dialog>
          <DialogTrigger render={<Button variant="outline" />}>
            Dialog
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Stop instance?</DialogTitle>
              <DialogDescription>
                The instance keeps its volumes and private address.
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <DialogClose render={<Button variant="outline" />}>
                Cancel
              </DialogClose>
              <DialogClose render={<Button />}>Stop</DialogClose>
            </DialogFooter>
          </DialogContent>
        </Dialog>
        <Sheet>
          <SheetTrigger render={<Button variant="outline" />}>
            Sheet
          </SheetTrigger>
          <SheetContent>
            <SheetHeader>
              <SheetTitle>i-0a1b2c3d4e5f67890</SheetTitle>
              <SheetDescription>
                The detail panel overlays the table on tablet widths.
              </SheetDescription>
            </SheetHeader>
          </SheetContent>
        </Sheet>
        <Popover>
          <PopoverTrigger render={<Button variant="outline" />}>
            Popover
          </PopoverTrigger>
          <PopoverContent>
            <PopoverHeader>
              <PopoverTitle>Filter by state</PopoverTitle>
              <PopoverDescription>Running (3), Stopped (1)</PopoverDescription>
            </PopoverHeader>
          </PopoverContent>
        </Popover>
        <HoverCard>
          <HoverCardTrigger render={<Button variant="link" />}>
            vpc-827b06b5680e9ca5d
          </HoverCardTrigger>
          <HoverCardContent>
            <span className="font-mono">172.31.0.0/16</span>, default VPC, one
            subnet.
          </HoverCardContent>
        </HoverCard>
        <Tooltip>
          <TooltipTrigger render={<Button variant="ghost" />}>
            Tooltip
          </TooltipTrigger>
          <TooltipContent>Copy instance ID</TooltipContent>
        </Tooltip>
        <DropdownMenu>
          <DropdownMenuTrigger render={<Button variant="outline" />}>
            Actions
          </DropdownMenuTrigger>
          <DropdownMenuContent>
            <DropdownMenuItem>Reboot</DropdownMenuItem>
            <DropdownMenuItem>Stop</DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive">Terminate</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </Specimen>
      <Specimen label="Command palette">
        <div className="w-full max-w-md rounded-xl border">
          <Command<SpecimenCommand> items={specimenCommands}>
            <CommandInput
              aria-label="Search resources and commands"
              placeholder="Search resources and commands"
            />
            <CommandEmpty>No results.</CommandEmpty>
            <CommandList>
              {(group: (typeof specimenCommands)[number]) => (
                <CommandGroup
                  heading={group.label}
                  items={group.items}
                  key={group.label}
                >
                  <CommandCollection>
                    {(item: SpecimenCommand) => (
                      <CommandItem key={item.label} value={item}>
                        {item.label}
                        {item.shortcut && (
                          <CommandShortcut>{item.shortcut}</CommandShortcut>
                        )}
                      </CommandItem>
                    )}
                  </CommandCollection>
                </CommandGroup>
              )}
            </CommandList>
          </Command>
        </div>
      </Specimen>
    </CatalogueSection>
  )
}
