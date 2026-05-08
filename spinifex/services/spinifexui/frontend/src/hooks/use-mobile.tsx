import { useSyncExternalStore } from "react"

const MOBILE_BREAKPOINT = 768
const MEDIA_QUERY = `(max-width: ${MOBILE_BREAKPOINT - 1}px)`
const mql = window.matchMedia(MEDIA_QUERY)

// oxlint-disable-next-line promise/prefer-await-to-callbacks -- useSyncExternalStore subscriber API requires a callback
function subscribe(callback: () => void): () => void {
  mql.addEventListener("change", callback)
  return () => mql.removeEventListener("change", callback)
}

function getSnapshot(): boolean {
  return mql.matches
}

export function useIsMobile(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot)
}
