import { ChevronDown, ChevronRight } from "lucide-react"
import { useState } from "react"
import type { UseFormReturn } from "react-hook-form"

import { Button } from "@/components/ui/button"
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

  if (count === 0) {
    return null
  }

  return (
    <div className="mt-2">
      <Button
        className="gap-1 px-0 text-xs text-muted-foreground"
        onClick={() => {
          setExpanded(!expanded)
        }}
        type="button"
        variant="link"
      >
        {expanded ? (
          <ChevronDown className="size-3" />
        ) : (
          <ChevronRight className="size-3" />
        )}
        Customize {type} subnet CIDR blocks
      </Button>

      {expanded && (
        <div className="mt-2 space-y-2">
          {Array.from({ length: count }, (_, i) => (
            <div className="flex items-center gap-2" key={i}>
              <span className="w-32 text-xs text-muted-foreground">
                {type === "public" ? "Public" : "Private"} subnet {i + 1}
              </span>
              <Input
                defaultValue={defaults[i]}
                placeholder={defaults[i]}
                {...form.register(`${fieldName}.${i}`)}
              />
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
