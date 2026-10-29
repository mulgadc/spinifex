import { z } from "zod"

export const PANEL_TABS = ["overview", "monitoring", "settings"] as const

export type PanelTab = (typeof PANEL_TABS)[number]

// The open panel lives in the URL, so a selection can be shared, reloaded and
// closed with Back. Defaults keep every param optional to links.
/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
export const panelSearchFields = {
  selected: z.string().default("").catch(""),
  tab: z.enum(PANEL_TABS).default("overview").catch("overview"),
  create: z.boolean().default(false).catch(false),
}
/* oxlint-enable promise/prefer-await-to-then */

export interface PanelSearch {
  selected: string
  tab: PanelTab
  create: boolean
}

// Also the defaults stripped from the URL, so a closed panel leaves no trace.
export const CLOSED_PANEL = {
  selected: "",
  tab: "overview",
  create: false,
} satisfies PanelSearch
