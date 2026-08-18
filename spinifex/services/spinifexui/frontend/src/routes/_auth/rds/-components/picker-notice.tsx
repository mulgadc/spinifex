export interface PickerNotice {
  tone: "muted" | "error"
  text: string
}

// An empty picker has three causes — not landed, failed, or genuinely empty —
// and only the last says anything about the cluster. Collapsing them makes an
// outage read as a capability the cluster does not have.
export function pickerNotice(
  query: { isPending: boolean; error: Error | null },
  isEmpty: boolean,
  messages: { loading: string; failed: string; empty: string },
): PickerNotice | undefined {
  if (query.isPending) {
    return { tone: "muted", text: messages.loading }
  }
  if (query.error) {
    return { tone: "error", text: `${messages.failed}: ${query.error.message}` }
  }
  if (isEmpty) {
    return { tone: "muted", text: messages.empty }
  }
  return undefined
}

export function PickerNoticeText({ notice }: { notice: PickerNotice }) {
  return (
    <p
      className={
        notice.tone === "error"
          ? "text-xs text-destructive"
          : "text-xs text-muted-foreground"
      }
    >
      {notice.text}
    </p>
  )
}
