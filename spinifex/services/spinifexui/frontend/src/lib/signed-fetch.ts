import { Sha256 } from "@aws-crypto/sha256-browser"
import { HttpRequest } from "@smithy/protocol-http"
import { SignatureV4 } from "@smithy/signature-v4"

import type { AwsCredentials } from "./auth"

const AWS_REGION = "ap-southeast-2"
const GATEWAY_PORT = 9999

interface SignedFetchOptions {
  action: string
  credentials: AwsCredentials
  service?: string
}

// SignedFetchError carries the AWS error code on `name` and the HTTP status, so
// that isStaleCredentialsError can classify it the same way it classifies SDK
// errors.
export class SignedFetchError extends Error {
  readonly status: number

  constructor(message: string, name: string, status: number) {
    super(message)
    // oxlint-disable-next-line unicorn/custom-error-definition -- name carries the AWS error code so isStaleCredentialsError can classify it
    this.name = name
    this.status = status
  }
}

function parseXmlTag(body: string, tag: string): string | null {
  const open = `<${tag}>`
  const close = `</${tag}>`
  const start = body.indexOf(open)
  if (start === -1) {
    return null
  }
  const end = body.indexOf(close, start + open.length)
  if (end === -1) {
    return null
  }
  return body.slice(start + open.length, end)
}

export async function signedFetch<T>({
  action,
  credentials,
  service = "spinifex",
}: SignedFetchOptions): Promise<T> {
  const protocol = window.location.protocol.replace(":", "")
  const body = `Action=${action}`

  // Sign the request against the real backend (localhost:9999) so the
  // gateway's SigV4 verification sees the host value it expects.
  const request = new HttpRequest({
    method: "POST",
    protocol,
    hostname: "localhost",
    port: GATEWAY_PORT,
    path: "/",
    headers: {
      host: `localhost:${GATEWAY_PORT}`,
      "content-type": "application/x-www-form-urlencoded",
    },
    body,
  })

  const signer = new SignatureV4({
    credentials: {
      accessKeyId: credentials.accessKeyId,
      secretAccessKey: credentials.secretAccessKey,
    },
    region: AWS_REGION,
    service,
    sha256: Sha256,
  })

  const signed = await signer.sign(request)

  const headers: Record<string, string> = {}
  for (const [key, value] of Object.entries(signed.headers)) {
    if (typeof value === "string") {
      headers[key] = value
    }
  }

  // Send the request through the same-origin reverse proxy instead of
  // directly to the gateway, eliminating cross-origin requests.
  const proxyUrl = `${window.location.protocol}//${window.location.host}/proxy/awsgw/`
  const response = await fetch(proxyUrl, {
    method: "POST",
    headers,
    body,
  })

  if (!response.ok) {
    const detail = await response.text().catch(() => "")
    const code = parseXmlTag(detail, "Code")
    const xmlMessage = parseXmlTag(detail, "Message")
    const summary = xmlMessage ?? detail
    throw new SignedFetchError(
      `${action} failed: ${response.status}${summary ? ` - ${summary}` : ""}`,
      code ?? "SignedFetchError",
      response.status,
    )
  }

  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- response.json() returns Promise<any>
  return await (response.json() as Promise<T>)
}
