import { type ReactNode, Suspense } from "react"

import { DetailPanel, PanelSpinner } from "@/components/detail-panel"

interface CreatePanelProps {
  // The form's name, e.g. "Create VPC", which titles and labels the panel.
  title: string
  onClose: () => void
  children: ReactNode
}

// Every create form opens in the wide side panel over its list, so the list
// stays in view and closing the panel, or Back, returns to it.
export function CreatePanel({ title, onClose, children }: CreatePanelProps) {
  return (
    <DetailPanel
      closeLabel="Close create form"
      label={title}
      onClose={onClose}
      size="wide"
      title={title}
    >
      <Suspense fallback={<PanelSpinner />}>{children}</Suspense>
    </DetailPanel>
  )
}
