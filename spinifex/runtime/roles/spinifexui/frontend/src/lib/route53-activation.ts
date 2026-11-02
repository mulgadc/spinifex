import { z } from "zod"

import { signedProxyXmlFetch } from "./signed-fetch"

// Activation is a Spinifex extension to Route 53 with no AWS SDK command, so
// it is called over REST-XML on the route53 signing service directly.
const ROUTE53_SERVICE = "route53"
const API_VERSION_PATH = "/2013-04-01"

export const ACTIVATION_STATES = [
  "PENDING_NAMESERVERS",
  "ACTIVE",
  "NAMESERVERS_LOST",
  "SUSPENDED",
] as const

export type ActivationState = (typeof ACTIVATION_STATES)[number]

const activationSchema = z.object({
  hostedZoneId: z.string(),
  state: z.enum(ACTIVATION_STATES),
  reason: z.string().optional(),
  nameServers: z.array(z.string()),
  checkedAt: z.string().optional(),
  delegatedTo: z.array(z.string()),
  delegationSource: z.string().optional(),
  override: z.string().optional(),
})

export type HostedZoneActivation = z.infer<typeof activationSchema>

// bareZoneId strips the "/hostedzone/" prefix ListHostedZones returns, leaving
// the ID the REST paths take.
export function bareZoneId(id: string): string {
  return id.replace(/^\/?hostedzone\//, "")
}

function childElement(parent: Element, name: string): Element | undefined {
  return [...parent.children].find((el) => el.localName === name)
}

function childText(parent: Element, name: string): string | undefined {
  const text = childElement(parent, name)?.textContent?.trim()

  return text === "" ? undefined : text
}

function childList(parent: Element, name: string): string[] {
  const list = childElement(parent, name)

  if (!list) {
    return []
  }

  return [...list.children].flatMap((el) => {
    const value = el.textContent?.trim() ?? ""

    return value === "" ? [] : [value]
  })
}

// parseActivationDocument reads a GetHostedZoneActivationResponse or
// VerifyHostedZoneDelegationResponse document.
export function parseActivationDocument(doc: Document): HostedZoneActivation {
  const root = doc.documentElement
  const activation = childElement(root, "Activation")

  if (!activation) {
    throw new Error(`${root.localName} carries no Activation`)
  }

  return activationSchema.parse({
    hostedZoneId: childText(root, "HostedZoneId") ?? "",
    state: childText(activation, "State"),
    reason: childText(activation, "Reason"),
    nameServers: childList(activation, "NameServers"),
    checkedAt: childText(activation, "CheckedAt"),
    delegatedTo: childList(activation, "DelegatedTo"),
    delegationSource: childText(activation, "DelegationSource"),
    override: childText(activation, "Override"),
  })
}

function activationPath(zoneId: string): string {
  return `${API_VERSION_PATH}/hostedzone/${encodeURIComponent(bareZoneId(zoneId))}/activation`
}

export async function getHostedZoneActivation(
  zoneId: string,
): Promise<HostedZoneActivation> {
  const doc = await signedProxyXmlFetch({
    label: "GetHostedZoneActivation",
    service: ROUTE53_SERVICE,
    method: "GET",
    path: activationPath(zoneId),
  })

  return parseActivationDocument(doc)
}

// verifyHostedZoneDelegation asks the gateway to check the parent's delegation
// now rather than waiting for the verifier's next pass.
export async function verifyHostedZoneDelegation(
  zoneId: string,
): Promise<HostedZoneActivation> {
  const doc = await signedProxyXmlFetch({
    label: "VerifyHostedZoneDelegation",
    service: ROUTE53_SERVICE,
    method: "POST",
    path: activationPath(zoneId),
  })

  return parseActivationDocument(doc)
}
