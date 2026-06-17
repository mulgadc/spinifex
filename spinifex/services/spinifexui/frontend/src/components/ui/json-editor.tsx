import type * as React from "react"

import { Textarea } from "@/components/ui/textarea"
import { formatJson } from "@/lib/json"
import { cn } from "@/lib/utils"

interface JsonEditorProps extends Omit<
  React.ComponentProps<typeof Textarea>,
  "onChange" | "value"
> {
  value: string
  onChange: (value: string) => void
  error?: boolean
}

// Controlled JSON textarea. On blur it pretty-prints valid JSON; malformed
// input is left untouched so the field's validation error stays visible. The
// plain value/onChange/error contract keeps it RHF-agnostic, so it works with
// Controller or local state and is testable in isolation.
export function JsonEditor({
  value,
  onChange,
  error,
  onBlur,
  className,
  ...props
}: JsonEditorProps) {
  const handleBlur = (event: React.FocusEvent<HTMLTextAreaElement>) => {
    const formatted = formatJson(value)
    if (formatted !== null && formatted !== value) {
      onChange(formatted)
    }
    onBlur?.(event)
  }

  return (
    <Textarea
      aria-invalid={error}
      className={cn("font-mono text-sm", className)}
      onBlur={handleBlur}
      onChange={(event) => onChange(event.target.value)}
      value={value}
      {...props}
    />
  )
}
