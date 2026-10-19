interface WizardFailure {
  created: { type: string; id: string | undefined }[]
  error?: Error
  failedStep?: string
}

// A wizard that stops part way leaves earlier resources in place, so the banner
// lists them alongside the step that failed.
export function WizardFailureBanner({
  result,
  createdHeading = "Successfully created resources:",
}: {
  result: WizardFailure | undefined
  createdHeading?: string
}) {
  if (!result?.error) {
    return null
  }

  return (
    <div className="mb-6 max-w-4xl rounded-md border border-destructive bg-destructive/10 p-4">
      <h2 className="text-sm font-medium text-destructive">
        Wizard failed: {result.failedStep}
      </h2>
      <p className="mt-1 text-sm text-destructive">{result.error.message}</p>
      {result.created.length > 0 && (
        <div className="mt-3">
          <p className="text-xs font-medium text-destructive">
            {createdHeading}
          </p>
          <ul className="mt-1 list-inside list-disc text-xs text-destructive">
            {result.created.map((r, i) => (
              // oxlint-disable-next-line no-array-index-key -- static failure summary, never reordered or filtered
              <li key={i}>
                {r.type}: {r.id ?? "(created)"}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}
