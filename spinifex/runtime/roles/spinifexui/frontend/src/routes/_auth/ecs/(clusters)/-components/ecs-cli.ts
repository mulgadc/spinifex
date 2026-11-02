import {
  type CommandPart,
  commandFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"

export const ECS_BIN = "AWS_PROFILE=spinifex aws ecs"

// A number input left empty reads as NaN, which is no value to show.
export function cliNumber(value: number, name: string): string {
  return Number.isFinite(value) ? String(value) : `<${name}>`
}

interface AwsvpcNetwork {
  subnets: string[]
  securityGroups: string[]
  assignPublicIp: boolean
}

// The shorthand for what RunTask and CreateService send in awsvpc mode, with
// securityGroups left out when none is chosen, as the request leaves it out.
export function awsvpcNetworkFlag({
  subnets,
  securityGroups,
  assignPublicIp,
}: AwsvpcNetwork): CommandPart[] {
  const fields = [`subnets=[${subnets.join(",")}]`]

  if (securityGroups.length > 0) {
    fields.push(`securityGroups=[${securityGroups.join(",")}]`)
  }

  fields.push(`assignPublicIp=${assignPublicIp ? "ENABLED" : "DISABLED"}`)

  return commandFlag(
    "\\\n  --network-configuration",
    shellSingleQuote(`awsvpcConfiguration={${fields.join(",")}}`),
  )
}
