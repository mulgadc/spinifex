import { CatchBoundary, type ErrorComponentProps } from "@tanstack/react-router"
import {
  ArrowLeft,
  Check,
  ChevronDown,
  Copy,
  LoaderCircle,
  type LucideIcon,
  X,
} from "lucide-react"
import { type ReactNode, useCallback, useEffect, useRef, useState } from "react"

import { ErrorBanner } from "@/components/error-banner"
import { InfoTip } from "@/components/info-tip"
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
  // "page" renders the same contents as the resource's own page, whatever the
  // width; "auto" is a side panel from lg up and a page below.
  layout?: "auto" | "page"
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

// Clicks on these inside the list do their own thing; anywhere else in the list
// pane closes the panel, as UniFi's does.
const LIST_CONTROLS =
  "a, button, input, select, textarea, label, tr, [role=checkbox], [role=combobox]"

// Faster than the slide in, so dismissing never feels like waiting.
const CLOSE_MS = 150

// The UniFi-style side panel: over the right of its list from lg up, and the
// whole page below that, where there is no room to share.
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
  layout = "auto",
  children,
}: DetailPanelProps) {
  const wideLayout = useWideLayout()
  const wide = layout === "auto" && wideLayout
  const sectionRef = useRef<HTMLElement>(null)
  const [closing, setClosing] = useState(false)

  // The panel slides out the way it slid in, then the route drops it.
  const close = useCallback(() => {
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)")

    if (!wide || reduced.matches) {
      onClose()
    } else {
      setClosing(true)
      window.setTimeout(onClose, CLOSE_MS)
    }
  }, [wide, onClose])

  // Escape closes the panel, unless it is closing a dialog or menu over it. A
  // page is somewhere the user navigated to, so it ignores Escape.
  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      const overlay = document.querySelector(
        "[role=dialog], [role=alertdialog], [role=menu], [role=listbox]",
      )

      if (
        layout === "auto" &&
        event.key === "Escape" &&
        !event.defaultPrevented &&
        !overlay
      ) {
        close()
      }
    }

    document.addEventListener("keydown", onKeyDown)

    return () => {
      document.removeEventListener("keydown", onKeyDown)
    }
  }, [layout, close])

  useEffect(() => {
    function onClick({ target }: MouseEvent) {
      if (
        wide &&
        target instanceof Element &&
        target.closest("main") &&
        !sectionRef.current?.contains(target) &&
        !target.closest(LIST_CONTROLS)
      ) {
        close()
      }
    }

    document.addEventListener("click", onClick)

    return () => {
      document.removeEventListener("click", onClick)
    }
  }, [wide, close])

  const current = tabs?.some((spec) => spec.id === tab) ? tab : tabs?.[0]?.id

  const body = tabs ? (
    <Tabs
      className="min-h-0 flex-1"
      onValueChange={(value) => {
        if (isPanelTab(value)) {
          onTabChange?.(value)
        }
      }}
      value={current}
    >
      {wide ? (
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
      ) : (
        <div className="overflow-x-auto">
          <TabsList>
            {tabs.map((spec) => (
              <TabsTab key={spec.id} value={spec.id}>
                {spec.label}
              </TabsTab>
            ))}
          </TabsList>
        </div>
      )}
      {tabs.map((spec) => (
        <TabsPanel
          className="min-h-0 flex-1 data-[layout=panel]:overflow-y-auto"
          data-layout={wide ? "panel" : "page"}
          key={spec.id}
          value={spec.id}
        >
          <div className="flex flex-col gap-3 px-3 pb-3 in-data-[layout=page]:px-0">
            <PanelBoundary resetKey={spec.id}>{spec.content}</PanelBoundary>
          </div>
        </TabsPanel>
      ))}
    </Tabs>
  ) : (
    <div className="min-h-0 flex-1 overflow-y-auto p-4 in-data-[layout=page]:px-0">
      <PanelBoundary resetKey={label}>{children}</PanelBoundary>
    </div>
  )

  if (!wide) {
    // The resource's own page: a standard heading, text tabs, one column.
    return (
      <section
        aria-label={label}
        className="mx-auto flex w-full max-w-5xl min-w-0 flex-col gap-4"
        data-layout="page"
        data-size={size}
        ref={sectionRef}
      >
        <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
          <div className="flex min-w-0 items-start gap-2">
            <Button
              aria-label={closeLabel}
              className="mt-1"
              onClick={onClose}
              size="icon-sm"
              variant="ghost"
            >
              <ArrowLeft />
            </Button>
            <div className="min-w-0">
              <h1 className="text-2xl font-semibold tracking-tight break-words">
                {title}
              </h1>
              {subtitle && (
                <p className="mt-1 font-mono text-xs text-muted-foreground">
                  {subtitle}
                </p>
              )}
            </div>
            {fullPage && <span className="mt-1">{fullPage}</span>}
          </div>
          {summary && (
            <div className="flex flex-wrap items-center gap-3">{summary}</div>
          )}
        </header>
        {body}
      </section>
    )
  }

  return (
    <section
      aria-label={label}
      className="fixed top-12 right-0 bottom-0 z-40 flex w-110 min-w-0 flex-col border-l bg-card transition duration-200 ease-out data-closing:translate-x-full data-closing:duration-150 data-closing:ease-in data-[size=wide]:w-150 motion-reduce:transition-none xl:w-120 data-[size=wide]:xl:w-170 starting:translate-x-full"
      data-closing={closing ? "" : undefined}
      data-layout="panel"
      data-size={size}
      ref={sectionRef}
    >
      {/* The shade on the list's side, so the panel reads as lying over it. */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-y-0 right-full w-10 bg-linear-to-l from-black/10 to-transparent dark:from-black/40"
      />
      <header className="flex items-center gap-2 border-b px-2 py-2">
        <span className="flex size-8 shrink-0 items-center justify-center">
          {fullPage}
        </span>
        <div className="min-w-0 flex-1 text-center">
          <h2 className="truncate text-sm font-semibold">{title}</h2>
          {subtitle && (
            <p className="truncate font-mono text-xs text-muted-foreground">
              {subtitle}
            </p>
          )}
        </div>
        <Button
          aria-label={closeLabel}
          onClick={close}
          size="icon-sm"
          variant="ghost"
        >
          <X />
        </Button>
      </header>

      {summary && (
        <div className="flex flex-col items-center gap-3 border-b px-4 py-3">
          {summary}
        </div>
      )}

      {body}
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
  // An (i) beside the label, for a value that needs context to be read.
  info?: ReactNode
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
  info,
}: PanelFieldProps) {
  return (
    <div className="flex items-center justify-between gap-4 py-1.5">
      <dt className="flex shrink-0 items-center gap-1 text-muted-foreground">
        {label}
        {info && <InfoTip label={label}>{info}</InfoTip>}
      </dt>
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

// What a panel shows while its form loads: a spinner on the panel's own
// background, so nothing flashes a grey block before the content.
export function PanelSpinner() {
  return (
    <output
      aria-label="Loading"
      className="flex min-h-64 flex-1 items-center justify-center"
    >
      <LoaderCircle className="size-6 animate-spin text-muted-foreground motion-reduce:animate-none" />
    </output>
  )
}
