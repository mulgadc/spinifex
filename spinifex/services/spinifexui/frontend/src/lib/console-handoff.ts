import {
  awsCredentialsSchema,
  setSessionCredentials,
  type AwsCredentialsInput,
} from "./auth"
import { clearClients } from "./awsClient"
import { exchangeForSession } from "./sts"

// Origins allowed to hand credentials to the console. Hardcoded on purpose —
// this is the control that stops an arbitrary page from opening the console
// and being handed a session. Never derive it from the message.
const ALLOWED_OPENER_ORIGINS = [
  "https://mulgadc.com",
  "https://staging.mulgadc.com",
]

const READY = "spx-handoff-ready"
const CREDS = "spx-handoff-creds"

/**
 * Credential handoff receiver for the login page.
 *
 * Lets mulgadc.com/signup drop the customer straight into the console signed
 * in, instead of asking them to paste a 40-character secret. Contract (sender:
 * mulgadc.com src/pages/signup.astro, `wireConsoleHandoff`):
 *
 *   1. mulgadc.com opens <console>/login?handoff=1
 *   2. we postMessage {type:'spx-handoff-ready'} to window.opener
 *   3. it replies {type:'spx-handoff-creds', accessKeyId, secretAccessKey}
 *   4. we exchange those for a session (the normal login path) and go to /
 *
 * The credentials never touch a URL, the DOM, or storage on the way in — they
 * go from the message straight into exchangeForSession, which is what keeps the
 * long-lived secret out of localStorage (only the short-lived STS session is
 * persisted).
 *
 * Every inbound message is checked three ways: origin in the allowlist, source
 * is our opener, and type is the expected one. The source check is the one that
 * matters most — without it any frame on an allowed origin could ask for the
 * credentials.
 *
 * Degrades silently: opened directly, or by an opener that doesn't implement
 * this, no message arrives and the normal form stays usable. Returns a cleanup
 * function that removes the listener.
 *
 * @param onSuccess called just before navigation, so the UI can show progress
 * @param onFailure called if credentials arrived but STS rejected them
 */
export function startConsoleHandoff(opts?: {
  onSuccess?: () => void
  onFailure?: () => void
}): () => void {
  // window.opener is typed `any` in lib.dom; launder it through `unknown`
  // so the assertion isn't flagged, and confirm it can receive a message.
  const openerRaw: unknown =
    typeof window === "undefined" ? null : window.opener
  const opener =
    openerRaw &&
    typeof (openerRaw as { postMessage?: unknown }).postMessage === "function"
      ? (openerRaw as Window)
      : null
  if (!opener) {
    return () => {}
  }

  let done = false

  const onMessage = async (ev: MessageEvent) => {
    if (done) {
      return
    }
    if (!ALLOWED_OPENER_ORIGINS.includes(ev.origin)) {
      return
    }
    if (ev.source !== opener) {
      return
    }
    const data = ev.data as { type?: string } | null
    if (!data || data.type !== CREDS) {
      return
    }

    // Validate the shape with the same schema the form uses before touching STS.
    const parsed = awsCredentialsSchema.safeParse(ev.data)
    if (!parsed.success) {
      return
    }

    done = true
    window.removeEventListener("message", onMessage)
    try {
      const session = await exchangeForSession(parsed.data)
      setSessionCredentials(session)
      clearClients()
      opts?.onSuccess?.()
      window.location.assign("/")
    } catch {
      // Credentials didn't validate against STS. Fall back to the form.
      opts?.onFailure?.()
    }
  }

  window.addEventListener("message", onMessage)

  // Announce readiness to each allowed opener origin. The ping carries no
  // secret, so posting to both is safe — only the real opener (whose origin
  // matches the targetOrigin) receives it, and it replies with the credentials.
  //
  // Re-announce on a short interval rather than firing once: the ping is
  // fire-and-forget with no ack, and the opener may not have attached its
  // listener yet when we mount (it opened us, so its tab is now backgrounded
  // and its timers/handlers can be briefly throttled). Retrying until the
  // credentials arrive makes the handoff robust to that race — which is the
  // difference between "works in a test" and "works when a real customer
  // opens a new tab from the signup page". Stops the instant creds arrive
  // (done) or after ~10s, and always on cleanup.
  const announce = () => {
    for (const origin of ALLOWED_OPENER_ORIGINS) {
      try {
        opener.postMessage({ type: READY }, origin)
      } catch {
        // opener gone or blocked — ignore, the form still works
      }
    }
  }
  announce()
  let attempts = 0
  const timer = setInterval(() => {
    attempts += 1
    if (done || attempts > 20) {
      clearInterval(timer)
      return
    }
    announce()
  }, 500)

  return () => {
    clearInterval(timer)
    window.removeEventListener("message", onMessage)
  }
}
