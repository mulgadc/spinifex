import { useSyncExternalStore } from "react"

// From lg up a detail panel sits beside its list; below it, the panel is the
// page. Kept in step with the lg breakpoint the layout classes use.
const mql = window.matchMedia("(min-width: 1024px)")

// oxlint-disable-next-line promise/prefer-await-to-callbacks -- useSyncExternalStore subscriber API requires a callback
function subscribe(callback: () => void): () => void {
  mql.addEventListener("change", callback)

  return () => {
    mql.removeEventListener("change", callback)
  }
}

function getSnapshot(): boolean {
  return mql.matches
}

export function useWideLayout(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot)
}
