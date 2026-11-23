import type { RouteTable } from "@aws-sdk/client-ec2"
import { z } from "zod"

import { shellSingleQuote } from "@/components/cli-command-panel"
import { getNameTag } from "@/lib/utils"

// AWS's default gateway endpoint policy, which grants everything the service allows.
export const FULL_ACCESS_POLICY = JSON.stringify(
  {
    Version: "2008-10-17",
    Statement: [
      { Effect: "Allow", Principal: "*", Action: "*", Resource: "*" },
    ],
  },
  null,
  2,
)

// One statement granting everything, and nothing else beside it.
const fullAccessStatement = z.strictObject({
  Effect: z.literal("Allow"),
  Principal: z.literal("*"),
  Action: z.literal("*"),
  Resource: z.literal("*"),
})

const fullAccessPolicy = z.object({
  Statement: z.union([fullAccessStatement, z.tuple([fullAccessStatement])]),
})

// Whether a policy is the default full-access one, whatever its layout.
export function isFullAccessPolicy(document: string | undefined): boolean {
  if (document === undefined) {
    return true
  }

  try {
    return fullAccessPolicy.safeParse(JSON.parse(document)).success
  } catch {
    return false
  }
}

// A policy as one line, for the API and the CLI alike: the editor's layout
// would count against the 20,480-character limit.
export function compactPolicy(document: string): string {
  try {
    return JSON.stringify(JSON.parse(document))
  } catch {
    return document
  }
}

// A Name tag as the CLI's --tag-specifications JSON, quoted for the shell, so
// a quote or comma in the name cannot change the command.
export function nameTagSpecificationArg(name: string): string {
  return shellSingleQuote(
    JSON.stringify([
      { ResourceType: "vpc-endpoint", Tags: [{ Key: "Name", Value: name }] },
    ]),
  )
}

// What ModifyVpcEndpoint must add and remove to move from one set to another.
export interface RouteTableChanges {
  add: string[]
  remove: string[]
}

export function routeTableChanges(
  current: readonly string[],
  next: readonly string[],
): RouteTableChanges {
  const was = new Set(current)
  const will = new Set(next)

  return {
    add: next.filter((id) => !was.has(id)),
    remove: current.filter((id) => !will.has(id)),
  }
}

// A route table's second line in a picker: its name, and what it serves.
export function routeTableDetail(table: RouteTable): string {
  const name = getNameTag(table.Tags)
  const main = table.Associations?.some((a) => a.Main) ?? false
  const subnets = (table.Associations ?? []).filter((a) => a.SubnetId).length

  let serves: string | undefined

  if (subnets === 1) {
    serves = "1 subnet"
  } else if (subnets > 1) {
    serves = `${subnets} subnets`
  }

  return [name, main ? "main" : undefined, serves]
    .filter((part) => part !== undefined)
    .join(" · ")
}
