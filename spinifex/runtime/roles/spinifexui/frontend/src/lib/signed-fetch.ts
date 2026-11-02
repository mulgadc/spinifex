import { Sha256 } from "@aws-crypto/sha256-browser"
import { HttpRequest } from "@smithy/protocol-http"
import { SignatureV4 } from "@smithy/signature-v4"
import { z } from "zod"

import { getCredentials } from "./auth"
import { getRegion } from "./cluster-config"

const GATEWAY_PORT = 9999

const FORM_CONTENT_TYPE = "application/x-www-form-urlencoded"
export const JSON_CONTENT_TYPE = "application/x-amz-json-1.1"

type ErrorParser = (
  label: string,
  status: number,
  detail: string,
) => SignedFetchError

interface SignedProxyFetchOptions {
  // label prefixes the thrown error, naming the action or JSON-1.1 target.
  label: string
  service: string
  // method defaults to POST; a GET carries no body and no content type.
  method?: "GET" | "POST"
  contentType?: string
  body?: string
  target?: string
  // path is the gateway path the request is signed for and proxied to.
  path?: string
  parseError?: ErrorParser
  signal?: AbortSignal
}

export interface SignedFetchOptions {
  action: string
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

// sendSigned SigV4-signs a request with the stored session against the gateway
// and sends it through the same-origin proxy, throwing parseError's result for
// a non-ok status. Callers differ only in how they read the body.
async function sendSigned({
  label,
  service,
  method = "POST",
  contentType,
  body,
  target,
  path = "/",
  parseError = errorFromBody,
  signal,
}: SignedProxyFetchOptions): Promise<Response> {
  const credentials = getCredentials()

  if (!credentials) {
    throw new Error("Not authenticated")
  }

  const protocol = window.location.protocol.replace(":", "")

  // Headers are set before signing so they are part of the signature, against
  // the real backend (localhost:9999) so the gateway's SigV4 verification sees
  // the host value it expects.
  const requestHeaders = {
    host: `localhost:${GATEWAY_PORT}`,
    ...(contentType && { "content-type": contentType }),
    ...(target && { "x-amz-target": target }),
  }

  const request = new HttpRequest({
    method,
    protocol,
    hostname: "localhost",
    port: GATEWAY_PORT,
    path,
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
  const proxyUrl = `${window.location.protocol}//${window.location.host}/proxy/awsgw${path}`

  const response = await fetch(proxyUrl, {
    method,
    headers,
    body,
    signal,
  })

  if (!response.ok) {
    const detail = await response.text().catch(() => "")
    throw parseError(label, response.status, detail)
  }

  return response
}

// signedProxyFetch sends a signed request through the gateway proxy and parses
// the JSON response.
export async function signedProxyFetch<T>(
  options: SignedProxyFetchOptions,
): Promise<T> {
  const response = await sendSigned(options)

  // The caller names the response shape its action returns, and the gateway has
  // already been checked for a non-ok status above.
  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- response.json() returns Promise<any>
  return await (response.json() as Promise<T>)
}

// signedProxyXmlFetch sends a signed request through the gateway proxy and
// parses the REST-XML response into a document.
export async function signedProxyXmlFetch(
  options: SignedProxyFetchOptions,
): Promise<Document> {
  const response = await sendSigned(options)
  const text = await response.text()
  const doc = new DOMParser().parseFromString(text, "application/xml")

  if (doc.querySelectorAll("parsererror").length > 0) {
    throw new SignedFetchError(
      `${options.label} failed: the response is not valid XML`,
      "SignedFetchError",
      response.status,
    )
  }

  return doc
}

export async function signedFetch<T>({
  action,
  service = "spinifex",
  params,
}: SignedFetchOptions): Promise<T> {
  const extraParams = params ? `&${new URLSearchParams(params).toString()}` : ""

  return await signedProxyFetch<T>({
    label: action,
    service,
    contentType: FORM_CONTENT_TYPE,
    body: `Action=${action}${extraParams}`,
  })
}

// signedStream sends a signed Action= request and returns the response unread,
// for an action that streams its body; abort the signal to end it.
export async function signedStream({
  action,
  service = "spinifex",
  params,
  signal,
}: SignedFetchOptions & { signal?: AbortSignal }): Promise<Response> {
  const extraParams = params ? `&${new URLSearchParams(params).toString()}` : ""

  return await sendSigned({
    label: action,
    service,
    contentType: FORM_CONTENT_TYPE,
    body: `Action=${action}${extraParams}`,
    signal,
  })
}

// ADMIN_JSON_CONTENT_TYPE is what the /admin surface expects, distinct from
// JSON_CONTENT_TYPE (JSON-1.1, used by the Action= target callers).
const ADMIN_JSON_CONTENT_TYPE = "application/json"

interface SignedAdminFetchOptions<TBody = Record<string, never>> {
  method: string
  body?: TBody
}

const adminErrorSchema = z.object({
  error: z
    .object({ code: z.string().optional(), message: z.string().optional() })
    .optional(),
  requestId: z.string().optional(),
})

// adminErrorFromBody reads the /admin surface's JSON error envelope
// (`{error:{code,message},requestId}`), which is shaped differently from the
// Action= and JSON-1.1 error bodies errorFromBody handles.
function adminErrorFromBody(
  label: string,
  status: number,
  detail: string,
): SignedFetchError {
  let code: string | undefined
  let message: string | undefined

  try {
    const parsed = adminErrorSchema.safeParse(JSON.parse(detail))

    if (parsed.success) {
      code = parsed.data.error?.code
      message = parsed.data.error?.message
    }
  } catch {
    // Non-JSON body.
  }

  return new SignedFetchError(
    `${label} failed: ${status}${message ? ` - ${message}` : ""}`,
    code ?? "SignedFetchError",
    status,
  )
}

// signedAdminFetch SigV4-signs and POSTs to the private JSON /admin/<Method>
// surface (mirrors the Go CLI's callAdmin), distinct from the Action= query
// surface signedFetch drives against path "/".
export async function signedAdminFetch<T, TBody = Record<string, never>>({
  method,
  body,
}: SignedAdminFetchOptions<TBody>): Promise<T> {
  return await signedProxyFetch<T>({
    label: method,
    service: "spinifex",
    contentType: ADMIN_JSON_CONTENT_TYPE,
    body: JSON.stringify(body ?? {}),
    path: `/admin/${method}`,
    parseError: adminErrorFromBody,
  })
}
