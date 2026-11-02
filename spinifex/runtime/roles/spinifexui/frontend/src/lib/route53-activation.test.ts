import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  clearCredentials,
  type SessionCredentials,
  setSessionCredentials,
} from "./auth"
import {
  bareZoneId,
  getHostedZoneActivation,
  parseActivationDocument,
  verifyHostedZoneDelegation,
} from "./route53-activation"

const credentials: SessionCredentials = {
  accessKeyId: "ASIAIOSFODNN7EXAMPLE",
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  sessionToken: "token",
  expiration: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
}

const NS = "https://route53.amazonaws.com/doc/2013-04-01/"

function activationXml(root: string, inner: string): string {
  return `<?xml version="1.0" encoding="UTF-8"?>
<${root} xmlns="${NS}">
  <HostedZoneId>Z0123</HostedZoneId>
  <Activation>${inner}</Activation>
</${root}>`
}

const PENDING_INNER = `
  <State>PENDING_NAMESERVERS</State>
  <Reason>parent delegates to other name servers</Reason>
  <NameServers><NameServer>ns1.spx3.net</NameServer><NameServer>ns2.spx3.net</NameServer></NameServers>
  <CheckedAt>2026-10-27T01:02:03Z</CheckedAt>
  <DelegatedTo><NameServer>ns1.registrar.example</NameServer></DelegatedTo>
  <DelegationSource>a.gtld-servers.net</DelegationSource>`

function parse(xml: string): Document {
  return new DOMParser().parseFromString(xml, "application/xml")
}

describe("parseActivationDocument", () => {
  it("reads every field of a namespaced response", () => {
    expect(
      parseActivationDocument(
        parse(activationXml("GetHostedZoneActivationResponse", PENDING_INNER)),
      ),
    ).toStrictEqual({
      hostedZoneId: "Z0123",
      state: "PENDING_NAMESERVERS",
      reason: "parent delegates to other name servers",
      nameServers: ["ns1.spx3.net", "ns2.spx3.net"],
      checkedAt: "2026-10-27T01:02:03Z",
      delegatedTo: ["ns1.registrar.example"],
      delegationSource: "a.gtld-servers.net",
      override: undefined,
    })
  })

  it("leaves absent lists empty and absent strings undefined", () => {
    const activation = parseActivationDocument(
      parse(
        activationXml(
          "VerifyHostedZoneDelegationResponse",
          "<State>ACTIVE</State><Override>activated by operator</Override>",
        ),
      ),
    )

    expect(activation.state).toBe("ACTIVE")
    expect(activation.nameServers).toStrictEqual([])
    expect(activation.delegatedTo).toStrictEqual([])
    expect(activation.reason).toBeUndefined()
    expect(activation.override).toBe("activated by operator")
  })

  it("rejects a document with no Activation", () => {
    expect(() =>
      parseActivationDocument(parse(`<Other xmlns="${NS}"/>`)),
    ).toThrow("Other carries no Activation")
  })

  it("rejects a state outside the four the API defines", () => {
    expect(() =>
      parseActivationDocument(
        parse(activationXml("R", "<State>MAYBE</State>")),
      ),
    ).toThrow(/PENDING_NAMESERVERS/)
  })
})

describe("bareZoneId", () => {
  it("strips the /hostedzone/ prefix and leaves a bare ID alone", () => {
    expect(bareZoneId("/hostedzone/Z0123")).toBe("Z0123")
    expect(bareZoneId("hostedzone/Z0123")).toBe("Z0123")
    expect(bareZoneId("Z0123")).toBe("Z0123")
  })
})

describe("activation requests", () => {
  beforeEach(() => {
    setSessionCredentials(credentials)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    clearCredentials()
  })

  it("GETs the activation path with the bare zone ID", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(
          activationXml("GetHostedZoneActivationResponse", PENDING_INNER),
        ),
      )

    vi.stubGlobal("fetch", fetchMock)

    const activation = await getHostedZoneActivation("/hostedzone/Z0123")

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toMatch(
      /\/proxy\/awsgw\/2013-04-01\/hostedzone\/Z0123\/activation$/,
    )
    expect(init.method).toBe("GET")
    expect(activation.state).toBe("PENDING_NAMESERVERS")
  })

  it("POSTs the same path to verify now", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(
          activationXml(
            "VerifyHostedZoneDelegationResponse",
            "<State>ACTIVE</State>",
          ),
        ),
      )

    vi.stubGlobal("fetch", fetchMock)

    const activation = await verifyHostedZoneDelegation("Z0123")

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toMatch(/\/hostedzone\/Z0123\/activation$/)
    expect(init.method).toBe("POST")
    expect(activation.state).toBe("ACTIVE")
  })
})
