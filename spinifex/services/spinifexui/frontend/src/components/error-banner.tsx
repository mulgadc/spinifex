export function ErrorBanner({ msg, error }: { msg?: string; error?: Error }) {
  let errorText: string | undefined
  if (error) {
    // A modelled AWS fault carries the reason in the message and only the code
    // in the name, so dropping either leaves the user without the half that
    // says which parameter was refused and why.
    if (error.name === "Error") {
      errorText = error.message
    } else {
      errorText =
        error.message === "" ? error.name : `${error.name}: ${error.message}`
    }
  }

  return (
    <div className="mb-6 max-w-4xl rounded-md border border-destructive bg-destructive/10 p-4">
      {msg && <h2 className="text-sm text-destructive">{msg}</h2>}
      {errorText && <p className="text-sm text-destructive">{errorText}</p>}
    </div>
  )
}
