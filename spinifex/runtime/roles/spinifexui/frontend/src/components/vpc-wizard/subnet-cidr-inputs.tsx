import { ChevronDown, ChevronRight } from "lucide-react"
import { useState } from "react"
import { type UseFormReturn, useFormState } from "react-hook-form"

import { Button } from "@/components/ui/button"
import { FieldError } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import type { CreateVpcWizardFormData } from "@/types/ec2"

interface SubnetCidrInputsProps {
  form: UseFormReturn<CreateVpcWizardFormData>
  type: "public" | "private"
  count: number
  defaults: string[]
}

export function SubnetCidrInputs({
  form,
  type,
  count,
  defaults,
}: SubnetCidrInputsProps) {
  const [expanded, setExpanded] = useState(false)

  const fieldName =
    type === "public" ? "publicSubnetCidrs" : "privateSubnetCidrs"

  const { errors } = useFormState({ control: form.control })
  const fieldErrors = errors[fieldName]
  // A subnet error must be visible, or Create appears to do nothing.
  const isOpen = expanded || fieldErrors !== undefined

  if (count === 0) {
    return null
  }

  return (
    <div className="mt-2">
      <Button
        onClick={() => {
          setExpanded(!expanded)
        }}
        type="button"
        size="inline"
        variant="link-muted"
      >
        {isOpen ? (
          <ChevronDown className="size-3" />
        ) : (
          <ChevronRight className="size-3" />
        )}
        Customize {type} subnet CIDR blocks
      </Button>

      {isOpen && (
        <div className="mt-2 space-y-2">
          {Array.from({ length: count }, (_, i) => (
            <div key={i}>
              <div className="flex items-center gap-2">
                <span className="w-32 shrink-0 text-xs text-muted-foreground">
                  {type === "public" ? "Public" : "Private"} subnet {i + 1}
                </span>
                {/* Left empty, the subnet uses the default shown as placeholder. */}
                <Input
                  aria-invalid={fieldErrors?.[i] !== undefined}
                  aria-label={`${type === "public" ? "Public" : "Private"} subnet ${i + 1} CIDR`}
                  placeholder={defaults[i]}
                  {...form.register(`${fieldName}.${i}`)}
                />
              </div>
              <FieldError className="ml-34" errors={[fieldErrors?.[i]]} />
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
