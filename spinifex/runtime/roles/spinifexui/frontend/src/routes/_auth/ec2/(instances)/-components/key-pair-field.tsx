import type { KeyPairInfo } from "@aws-sdk/client-ec2"
import { KeyRound, Upload } from "lucide-react"
import { useId } from "react"
import { type Control, Controller, type FieldErrors } from "react-hook-form"

import { Button } from "@/components/ui/button"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import type { CreateInstanceFormData } from "@/types/ec2"

import { CreateKeyPairPage } from "../../(key)/-components/create-key-pair-page"
import { ImportKeyPairPage } from "../../(key)/-components/import-key-pair-page"
import type { RunInstancesFormInput } from "./run-instances-helpers"

export type KeyPairFlow = "create" | "import"

const FLOW_TITLES = {
  create: "Create key pair",
  import: "Import public key",
} as const satisfies Record<KeyPairFlow, string>

interface KeyPairFieldProps {
  control: Control<RunInstancesFormInput, unknown, CreateInstanceFormData>
  errors: FieldErrors<RunInstancesFormInput>
  keyPairs: readonly KeyPairInfo[]
  onStartFlow: (flow: KeyPairFlow) => void
}

export function KeyPairField({
  control,
  errors,
  keyPairs,
  onStartFlow,
}: KeyPairFieldProps) {
  return (
    <Field>
      <FieldTitle>
        <label htmlFor="keyName">Key Pair</label>
      </FieldTitle>
      <Controller
        control={control}
        name="keyName"
        render={({ field }) => (
          <Select onValueChange={field.onChange} value={field.value ?? ""}>
            <SelectTrigger
              aria-invalid={!!errors.keyName}
              className="w-full"
              id="keyName"
            >
              <SelectValue placeholder="Select a key pair" />
            </SelectTrigger>
            <SelectContent>
              {keyPairs.map((keyPair) => (
                <SelectItem
                  key={keyPair.KeyPairId}
                  value={keyPair.KeyName ?? ""}
                >
                  {keyPair.KeyName}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      />
      {keyPairs.length === 0 && (
        <FieldDescription>
          No key pairs yet. Create one, or import your own public key.
        </FieldDescription>
      )}
      <div className="flex flex-wrap gap-2">
        <Button
          onClick={() => {
            onStartFlow("create")
          }}
          size="sm"
          type="button"
          variant="outline"
        >
          <KeyRound />
          {FLOW_TITLES.create}
        </Button>
        <Button
          onClick={() => {
            onStartFlow("import")
          }}
          size="sm"
          type="button"
          variant="outline"
        >
          <Upload />
          {FLOW_TITLES.import}
        </Button>
      </div>
      <FieldError errors={[errors.keyName]} />
    </Field>
  )
}

interface KeyPairFlowSectionProps {
  flow: KeyPairFlow
  // Called with the new key's ID and name on success, or nothing on cancel.
  onClose: (keyPairId?: string, keyName?: string) => void
}

// Shown in the launch panel in place of the launch form, which stays mounted
// behind it so every entry survives the detour.
export function KeyPairFlowSection({ flow, onClose }: KeyPairFlowSectionProps) {
  const headingId = useId()

  return (
    <section aria-labelledby={headingId} className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        <h3 className="text-sm font-medium" id={headingId}>
          {FLOW_TITLES[flow]}
        </h3>
        <p className="text-xs text-muted-foreground">
          Your instance settings are kept. The new key is selected when you
          return.
        </p>
      </div>
      {flow === "create" ? (
        <CreateKeyPairPage onClose={onClose} />
      ) : (
        <ImportKeyPairPage onClose={onClose} />
      )}
    </section>
  )
}
