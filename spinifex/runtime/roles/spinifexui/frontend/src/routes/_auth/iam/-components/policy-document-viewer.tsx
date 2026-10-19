import { formatJson } from "@/lib/json"

interface PolicyDocumentViewerProps {
  document: string
}

export function PolicyDocumentViewer({ document }: PolicyDocumentViewerProps) {
  return (
    <pre className="overflow-auto rounded-md border bg-muted p-4 font-mono text-sm">
      {formatJson(document) ?? document}
    </pre>
  )
}
