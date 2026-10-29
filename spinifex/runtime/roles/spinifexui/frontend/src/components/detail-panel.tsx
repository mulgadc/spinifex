import { CatchBoundary, type ErrorComponentProps } from "@tanstack/react-router"
import {
  ArrowLeft,
  Check,
  ChevronDown,
  Copy,
  type LucideIcon,
  X,
} from "lucide-react"
import { type ReactNode, useEffect } from "react"

import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { useCopyToClipboard } from "@/hooks/use-copy-to-clipboard"
import { useWideLayout } from "@/hooks/use-wide-layout"
import { PANEL_TABS, type PanelTab } from "@/lib/panel-search"

export interface PanelTabSpec {
  id: PanelTab
  label: string
  icon: LucideIcon
  content: ReactNode
}

interface DetailPanelProps {
  // Names the region, e.g. "Instance details", for screen readers and tests.
  label: string
  title: ReactNode
  subtitle?: string
  // Under the title: the state badge and the quick actions.
  summary?: ReactNode
  // A link to the resource's own page, which outlives the list it came from.
  fullPage?: ReactNode
  closeLabel: string
  onClose: () => void
  tabs?: readonly PanelTabSpec[]
  tab?: PanelTab
  onTabChange?: (tab: PanelTab) => void
  // Create forms need more room than a summary does.
  size?: "default" | "wide"
  children?: ReactNode
}

function PanelError({ error }: ErrorComponentProps) {
  return (
    <ErrorBanner
      error={error instanceof Error ? error : undefined}
      msg="This panel could not load"
    />
  )
}

// A failed query inside the panel stays in the panel, and the list beside it
// keeps working.
function PanelBoundary({
  resetKey,
  children,
}: {
  resetKey: string
  children: ReactNode
}) {
  return (
    <CatchBoundary errorComponent={PanelError} getResetKey={() => resetKey}>
      {children}
    </CatchBoundary>
  )
}

function isPanelTab(value: unknown): value is PanelTab {
  return PANEL_TABS.some((tab) => tab === value)
}

// The UniFi-style side panel: beside its list from lg up, and the whole page
// below that, where there is no room to share.
export function DetailPanel({
  label,
  title,
  subtitle,
  summary,
  fullPage,
  closeLabel,
  onClose,
  tabs,
  tab = "overview",
  onTabChange,
  size = "default",
  children,
}: DetailPanelProps) {
  const wide = useWideLayout()

  // Escape closes the panel, unless it is closing a dialog or menu over it.
  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      const overlay = document.querySelector(
        "[role=dialog], [role=alertdialog], [role=menu], [role=listbox]",
      )

      if (event.key === "Escape" && !event.defaultPrevented && !overlay) {
        onClose()
      }
    }

    document.addEventListener("keydown", onKeyDown)

    return () => {
      document.removeEventListener("keydown", onKeyDown)
    }
  }, [onClose])
  const current = tabs?.some((spec) => spec.id === tab) ? tab : tabs?.[0]?.id

  return (
    <section
      aria-label={label}
      className="flex min-w-0 flex-col data-[layout=panel]:sticky data-[layout=panel]:top-4 data-[layout=panel]:max-h-[calc(100svh-2rem)] data-[layout=panel]:w-100 data-[layout=panel]:shrink-0 data-[layout=panel]:overflow-hidden data-[layout=panel]:rounded-lg data-[layout=panel]:border data-[layout=panel]:bg-card data-[layout=panel]:transition data-[layout=panel]:duration-200 data-[layout=panel]:ease-out data-[size=wide]:data-[layout=panel]:w-140 data-[layout=panel]:xl:w-110 data-[size=wide]:data-[layout=panel]:xl:w-160 data-[layout=panel]:starting:translate-x-4 data-[layout=panel]:starting:opacity-0"
      data-layout={wide ? "panel" : "page"}
      data-size={size}
    >
      <header className="flex items-center gap-2 border-b px-2 py-2">
        {wide ? (
          <span className="flex size-8 shrink-0 items-center justify-center">
            {fullPage}
          </span>
        ) : (
          <Button
            aria-label={closeLabel}
            onClick={onClose}
            size="icon-sm"
            variant="ghost"
          >
            <ArrowLeft />
          </Button>
        )}
        <div className="min-w-0 flex-1 text-center">
          <h2 className="truncate text-sm font-semibold">{title}</h2>
          {subtitle && (
            <p className="truncate font-mono text-xs text-muted-foreground">
              {subtitle}
            </p>
          )}
        </div>
        {wide ? (
          <Button
            aria-label={closeLabel}
            onClick={onClose}
            size="icon-sm"
            variant="ghost"
          >
            <X />
          </Button>
        ) : (
          <span className="flex size-8 shrink-0 items-center justify-center">
            {fullPage}
          </span>
        )}
      </header>

      {summary && (
        <div className="flex flex-col items-center gap-3 border-b px-4 py-3">
          {summary}
        </div>
      )}

      {tabs ? (
        <Tabs
          className="min-h-0 flex-1"
          onValueChange={(value) => {
            if (isPanelTab(value)) {
              onTabChange?.(value)
            }
          }}
          value={current}
        >
          <TabsList variant="icon">
            {tabs.map((spec) => (
              <Tooltip key={spec.id}>
                <TooltipTrigger
                  render={
                    <TabsTab
                      aria-label={spec.label}
                      value={spec.id}
                      variant="icon"
                    />
                  }
                >
                  <spec.icon />
                </TooltipTrigger>
                <TooltipContent side="bottom">{spec.label}</TooltipContent>
              </Tooltip>
            ))}
          </TabsList>
          {tabs.map((spec) => (
            <TabsPanel
              className="min-h-0 flex-1 overflow-y-auto"
              key={spec.id}
              value={spec.id}
            >
              <div className="flex flex-col gap-3 px-3 pb-3">
                <PanelBoundary resetKey={spec.id}>{spec.content}</PanelBoundary>
              </div>
            </TabsPanel>
          ))}
        </Tabs>
      ) : (
        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          <PanelBoundary resetKey={label}>{children}</PanelBoundary>
        </div>
      )}
    </section>
  )
}

interface PanelSectionProps {
  title: string
  children: ReactNode
  defaultOpen?: boolean
}

// A folding card inside the panel, as UniFi groups Services, Manage, History.
export function PanelSection({
  title,
  children,
  defaultOpen = true,
}: PanelSectionProps) {
  return (
    <div className="rounded-lg border bg-background/40">
      <Collapsible defaultOpen={defaultOpen}>
        <CollapsibleTrigger variant="section">
          {title}
          <ChevronDown
            aria-hidden
            className="size-4 transition-transform duration-100 group-data-[panel-open]/section:rotate-180"
          />
        </CollapsibleTrigger>
        <CollapsibleContent variant="section">{children}</CollapsibleContent>
      </Collapsible>
    </div>
  )
}

export function PanelFields({ children }: { children: ReactNode }) {
  return <dl className="divide-y text-sm">{children}</dl>
}

interface PanelFieldProps {
  label: string
  children?: ReactNode
  // IDs and addresses read better, and copy cleaner, in the mono face.
  mono?: boolean
  // The value a copy button puts on the clipboard, for IDs and addresses.
  copy?: string
}

function CopyValue({ label, value }: { label: string; value: string }) {
  const { copied, copy } = useCopyToClipboard()

  return (
    <Button
      aria-label={copied ? `${label} copied` : `Copy ${label}`}
      onClick={() => {
        void copy(value)
      }}
      size="icon-xs"
      variant="ghost"
    >
      {copied ? <Check /> : <Copy />}
    </Button>
  )
}

export function PanelField({
  label,
  children,
  mono = false,
  copy,
}: PanelFieldProps) {
  return (
    <div className="flex items-center justify-between gap-4 py-1.5">
      <dt className="shrink-0 text-muted-foreground">{label}</dt>
      <dd
        className="flex min-w-0 items-center justify-end gap-1 text-right data-mono:font-mono data-mono:text-xs"
        data-mono={mono ? "" : undefined}
      >
        <span className="min-w-0 truncate">
          {children ?? <span className="text-muted-foreground">—</span>}
        </span>
        {copy && <CopyValue label={label} value={copy} />}
      </dd>
    </div>
  )
}

interface PanelTagsProps {
  tags: readonly { Key?: string; Value?: string }[] | undefined
}

export function PanelTags({ tags }: PanelTagsProps) {
  return (
    <PanelSection title="Tags">
      {tags?.length ? (
        <PanelFields>
          {tags.map((tag) => (
            <PanelField key={tag.Key} label={tag.Key ?? ""}>
              {tag.Value}
            </PanelField>
          ))}
        </PanelFields>
      ) : (
        <p className="text-sm text-muted-foreground">No tags.</p>
      )}
    </PanelSection>
  )
}
