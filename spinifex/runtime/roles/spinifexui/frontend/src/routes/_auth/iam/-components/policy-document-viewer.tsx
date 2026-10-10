interface PolicyDocumentViewerProps {
  document: string
}

export function PolicyDocumentViewer({ document }: PolicyDocumentViewerProps) {
  let formatted: string
  try {
    formatted = JSON.stringify(JSON.parse(document), null, 2)
  } catch {
    formatted = document
  }

  return (
    <pre className="overflow-auto rounded-md border bg-muted p-4 font-mono text-sm">
      {formatted}
    </pre>
  )
}
