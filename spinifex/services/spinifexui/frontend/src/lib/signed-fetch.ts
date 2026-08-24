import { Sha256 } from "@aws-crypto/sha256-browser"
import { HttpRequest } from "@smithy/protocol-http"
import { SignatureV4 } from "@smithy/signature-v4"
import { z } from "zod"

import type { SessionCredentials } from "./auth"
import { getRegion } from "./cluster-config"

const GATEWAY_PORT = 9999

export const FORM_CONTENT_TYPE = "application/x-www-form-urlencoded"
export const JSON_CONTENT_TYPE = "application/x-amz-json-1.1"

interface SignedProxyFetchOptions {
  // label prefixes the thrown error, naming the action or JSON-1.1 target.
  label: string
  credentials: SessionCredentials
  service: string
  contentType: string
  body: string
  target?: string
}

interface SignedFetchOptions {
  action: string
  credentials: SessionCredentials
  service?: string
  params?: Record<string, string>
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

// Gateways spell the field either way, so accept both and let the caller take
// whichever one the body carried.
const jsonErrorSchema = z.object({
  message: z.string().optional(),
  Message: z.string().optional(),
})

// jsonErrorMessage extracts a human-readable message from a JSON-1.1 error
// body. Returns null when the body is not JSON or carries no message.
function jsonErrorMessage(body: string): string | null {
  try {
    const parsed = jsonErrorSchema.safeParse(JSON.parse(body))
    if (parsed.success) {
      return parsed.data.message ?? parsed.data.Message ?? null
    }
  } catch {
    // Non-JSON body.
  }
  return null
}

function errorFromBody(
  label: string,
  status: number,
  detail: string,
): SignedFetchError {
  const code = parseXmlTag(detail, "Code")
  const summary =
    parseXmlTag(detail, "Message") ?? jsonErrorMessage(detail) ?? detail
  return new SignedFetchError(
    `${label} failed: ${status}${summary ? ` - ${summary}` : ""}`,
    code ?? "SignedFetchError",
    status,
  )
}

// signedProxyFetch SigV4-signs a request against the gateway and POSTs it
// through the same-origin proxy. Query-form and JSON-1.1 callers differ only in
// their content type, target header and error body shape.
export async function signedProxyFetch<T>({
  label,
  credentials,
  service,
  contentType,
  body,
  target,
}: SignedProxyFetchOptions): Promise<T> {
  const protocol = window.location.protocol.replace(":", "")

  // Headers are set before signing so they are part of the signature, against
  // the real backend (localhost:9999) so the gateway's SigV4 verification sees
  // the host value it expects.
  const baseHeaders = {
    host: `localhost:${GATEWAY_PORT}`,
    "content-type": contentType,
  }
  const requestHeaders = target
    ? { ...baseHeaders, "x-amz-target": target }
    : baseHeaders

  const request = new HttpRequest({
    method: "POST",
    protocol,
    hostname: "localhost",
    port: GATEWAY_PORT,
    path: "/",
    headers: requestHeaders,
    body,
  })

  const signer = new SignatureV4({
    credentials: {
      accessKeyId: credentials.accessKeyId,
      secretAccessKey: credentials.secretAccessKey,
      // SignatureV4 emits the X-Amz-Security-Token header when present; the
      // gateway's ASIA path verifies it.
      sessionToken: credentials.sessionToken,
    },
    region: getRegion(),
    service,
    sha256: Sha256,
  })

  const signed = await signer.sign(request)

  const headers = { ...signed.headers }

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
    throw errorFromBody(label, response.status, detail)
  }

  // SAFETY: the caller names the response shape its action returns, and the
  // gateway has already been checked for a non-ok status above.
  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- response.json() returns Promise<any>
  return await (response.json() as Promise<T>)
}

export async function signedFetch<T>({
  action,
  credentials,
  service = "spinifex",
  params,
}: SignedFetchOptions): Promise<T> {
  const extraParams = params ? `&${new URLSearchParams(params).toString()}` : ""
  return await signedProxyFetch<T>({
    label: action,
    credentials,
    service,
    contentType: FORM_CONTENT_TYPE,
    body: `Action=${action}${extraParams}`,
  })
}
