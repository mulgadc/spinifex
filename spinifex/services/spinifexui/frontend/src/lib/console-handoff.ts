import { z } from "zod"

import { awsCredentialsSchema, setSessionCredentials } from "./auth"
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

// The whole inbound message is parsed here, at the I/O boundary, so nothing
// downstream branches on the shape of an untrusted value.
const handoffMessageSchema = awsCredentialsSchema.extend({
  type: z.literal(CREDS),
})

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
 * function that removes the listener, or undefined when there was no opener to
 * listen to and so nothing was attached.
 *
 * @param onSuccess called just before navigation, so the UI can show progress
 * @param onFailure called if credentials arrived but STS rejected them
 */
export function startConsoleHandoff(opts?: {
  onSuccess?: () => void
  onFailure?: () => void
}): (() => void) | undefined {
  // window.opener is a cross-realm WindowProxy typed `any` by lib.dom, so
  // instanceof is unreliable across realms and a duck check is the only one
  // that holds for the real sender.
  const openerRaw: unknown = globalThis.window?.opener ?? null
  /* oxlint-disable anti-slop/no-runtime-typeof, typescript/no-unsafe-type-assertion -- a cross-realm WindowProxy carries no narrower contract, so a duck check is the only reliable one */
  const canPost =
    typeof (openerRaw as { postMessage?: unknown })?.postMessage === "function"
  const opener = canPost ? (openerRaw as Window) : null
  /* oxlint-enable anti-slop/no-runtime-typeof, typescript/no-unsafe-type-assertion */
  if (!opener) {
    return undefined
  }

  let done = false

  const handleMessage = async (ev: MessageEvent) => {
    if (done) {
      return
    }
    if (!ALLOWED_OPENER_ORIGINS.includes(ev.origin)) {
      return
    }
    if (ev.source !== opener) {
      return
    }

    // Shape and type are both established here; a message that fails is simply
    // not ours, so it is dropped without touching STS.
    const parsed = handoffMessageSchema.safeParse(ev.data)
    if (!parsed.success) {
      return
    }

    done = true
    window.removeEventListener("message", onMessage)
    try {
      const session = await exchangeForSession({
        accessKeyId: parsed.data.accessKeyId,
        secretAccessKey: parsed.data.secretAccessKey,
      })
      setSessionCredentials(session)
      clearClients()
      opts?.onSuccess?.()
      window.location.assign("/")
    } catch {
      // Credentials didn't validate against STS. Fall back to the form.
      opts?.onFailure?.()
    }
  }

  // The listener must return void, so the async work is kicked off rather than
  // returned; nothing awaits it and failures are handled inside.
  const onMessage = (ev: MessageEvent) => {
    void handleMessage(ev)
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
