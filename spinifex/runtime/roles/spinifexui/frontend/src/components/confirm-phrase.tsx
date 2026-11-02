import { useId } from "react"

import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

interface ConfirmPhraseProps {
  phrase: string
  value: string
  onChange: (value: string) => void
}

// Whether the typed value matches, ignoring case and surrounding space.
export function phraseConfirmed(phrase: string | undefined, value: string) {
  return !phrase || value.trim().toLowerCase() === phrase.toLowerCase()
}

// The typed confirmation for deletes that cannot be walked back, as AWS asks
// for "delete" before removing a volume. The confirm stays disabled until it
// matches.
export function ConfirmPhrase({ phrase, value, onChange }: ConfirmPhraseProps) {
  const id = useId()

  return (
    <Field>
      <FieldLabel htmlFor={id}>
        To confirm, type “{phrase}” in this field.
      </FieldLabel>
      <Input
        autoComplete="off"
        id={id}
        onChange={(event) => {
          onChange(event.target.value)
        }}
        placeholder={phrase}
        spellCheck={false}
        value={value}
      />
    </Field>
  )
}
