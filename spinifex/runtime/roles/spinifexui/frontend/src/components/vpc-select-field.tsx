import type { Vpc } from "@aws-sdk/client-ec2"
import type { FieldValues } from "react-hook-form"

import { SelectField, type SelectFieldProps } from "@/components/form-fields"
import { vpcLabel } from "@/lib/utils"

type VpcSelectFieldProps<T extends FieldValues> = Omit<
  SelectFieldProps<T>,
  "label" | "options"
> & {
  vpcs: Vpc[]
  label?: string
}

export function VpcSelectField<T extends FieldValues>({
  vpcs,
  label = "VPC",
  ...props
}: VpcSelectFieldProps<T>) {
  return (
    <SelectField
      {...props}
      label={label}
      options={vpcs.map((vpc) => ({
        value: vpc.VpcId ?? "",
        label: vpcLabel(vpc),
      }))}
    />
  )
}
