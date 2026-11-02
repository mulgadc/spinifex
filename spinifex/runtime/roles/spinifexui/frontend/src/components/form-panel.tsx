import { ArrowLeft, X } from "lucide-react"
import { type ReactNode, Suspense, useEffect, useRef } from "react"
import { createPortal } from "react-dom"

import {
  escapeOwned,
  PanelBoundary,
  PanelSpinner,
} from "@/components/detail-panel"
import { Button } from "@/components/ui/button"
import { useWideLayout } from "@/hooks/use-wide-layout"

interface FormPanelProps {
  // The form's name, e.g. "Attach volume", which titles and labels the panel.
  title: string
  // The resource the form acts on, set in mono beneath the title.
  subtitle?: string
  onClose: () => void
  children: ReactNode
}

// A form opened from inside a resource's panel or page. It lies over that
// panel at the same edge, or covers the page below lg, and closing it goes
// back to the resource.
export function FormPanel({
  title,
  subtitle,
  onClose,
  children,
}: FormPanelProps) {
  const wide = useWideLayout()
  const headingRef = useRef<HTMLHeadingElement>(null)

  useEffect(() => {
    const opener = document.activeElement

    headingRef.current?.focus({ preventScroll: true })

    return () => {
      if (opener instanceof HTMLElement && opener.isConnected) {
        opener.focus({ preventScroll: true })
      }
    }
  }, [])

  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if (
        event.key === "Escape" &&
        !event.defaultPrevented &&
        !escapeOwned(event.target, { formPanel: true })
      ) {
        onClose()
      }
    }

    document.addEventListener("keydown", onKeyDown)

    return () => {
      document.removeEventListener("keydown", onKeyDown)
    }
  }, [onClose])

  return createPortal(
    <section
      aria-label={title}
      className="fixed inset-x-0 top-12 bottom-0 z-50 flex min-w-0 flex-col bg-background data-wide:left-auto data-wide:w-150 data-wide:border-l data-wide:bg-card data-wide:xl:w-170"
      data-form-panel=""
      data-layout="panel"
      data-wide={wide ? "" : undefined}
    >
      <header className="flex items-center gap-2 border-b px-2 py-2">
        <span className="flex size-8 shrink-0 items-center justify-center">
          {!wide && (
            <Button
              aria-label={`Back from ${title}`}
              onClick={onClose}
              size="icon-sm"
              variant="ghost"
            >
              <ArrowLeft />
            </Button>
          )}
        </span>
        <div className="min-w-0 flex-1 text-center">
          <h2
            className="truncate text-sm font-semibold outline-none"
            ref={headingRef}
            tabIndex={-1}
          >
            {title}
          </h2>
          {subtitle && (
            <p className="truncate font-mono text-xs text-muted-foreground">
              {subtitle}
            </p>
          )}
        </div>
        <span className="flex size-8 shrink-0 items-center justify-center">
          {wide && (
            <Button
              aria-label={`Close ${title}`}
              onClick={onClose}
              size="icon-sm"
              variant="ghost"
            >
              <X />
            </Button>
          )}
        </span>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto p-4">
        <PanelBoundary resetKey={title}>
          <Suspense fallback={<PanelSpinner />}>{children}</Suspense>
        </PanelBoundary>
      </div>
    </section>,
    document.body,
  )
}
