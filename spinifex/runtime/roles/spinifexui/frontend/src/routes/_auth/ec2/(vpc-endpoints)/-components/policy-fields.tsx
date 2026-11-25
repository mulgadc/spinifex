import { FileJson, ShieldCheck } from "lucide-react"

import { ChoiceTile, ChoiceTiles } from "@/components/choice-tile"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { JsonEditor } from "@/components/ui/json-editor"

export type PolicyMode = "full" | "custom"

interface PolicyFieldsProps {
  mode: PolicyMode
  onModeChange: (mode: PolicyMode) => void
  document: string
  onDocumentChange: (document: string) => void
  error?: { message?: string }
}

// AWS's choice between the default full-access policy and a custom document.
export function PolicyFields({
  mode,
  onModeChange,
  document,
  onDocumentChange,
  error,
}: PolicyFieldsProps) {
  return (
    <>
      <Field>
        <FieldTitle>Policy</FieldTitle>
        <ChoiceTiles columns={2} label="Policy">
          <ChoiceTile
            checked={mode === "full"}
            description="Any request the caller's own policies allow"
            icon={<ShieldCheck aria-hidden className="size-4" />}
            label="Full access"
            layout="inline"
            onSelect={() => {
              onModeChange("full")
            }}
          />
          <ChoiceTile
            checked={mode === "custom"}
            description="Only what the document allows"
            icon={<FileJson aria-hidden className="size-4" />}
            label="Custom"
            layout="inline"
            onSelect={() => {
              onModeChange("custom")
            }}
          />
        </ChoiceTiles>
      </Field>
      {mode === "custom" && (
        <Field>
          <FieldTitle>
            <label htmlFor="policyDocument">Policy document</label>
          </FieldTitle>
          <JsonEditor
            error={!!error}
            id="policyDocument"
            onChange={onDocumentChange}
            rows={12}
            size="sm"
            value={document}
          />
          <FieldError errors={[error]} />
        </Field>
      )}
    </>
  )
}
