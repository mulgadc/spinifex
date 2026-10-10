// ScanFindings is the Scan tab of the repository detail page. mulga's registry
// runs no vulnerability scanner, so the ECR scan API returns
// OperationNotSupportedException. Rather than fire a request that surfaces a red
// error, the tab states plainly that scanning is unavailable.
export function ScanFindings() {
  return (
    <div className="rounded-lg border bg-card p-4">
      <h2 className="mb-2 text-sm font-medium">Image scanning</h2>
      <p className="text-sm text-muted-foreground">
        Vulnerability scanning is not supported by this registry. Pushed images
        are stored and served without scan findings.
      </p>
    </div>
  )
}
