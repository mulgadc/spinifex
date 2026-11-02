import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"

// A launch opens over the selected cluster's panel, and closing it returns
// there, so it rides beside `selected` rather than replacing it.
export const CLUSTER_LAUNCHES = ["", "service", "task"] as const

export type ClusterLaunch = (typeof CLUSTER_LAUNCHES)[number]

/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
export const clustersSearchSchema = z.object({
  ...panelSearchFields,
  launch: z.enum(CLUSTER_LAUNCHES).default("").catch(""),
})
/* oxlint-enable promise/prefer-await-to-then */

export interface ClustersSearch extends PanelSearch {
  launch: ClusterLaunch
}

export const CLOSED_CLUSTERS_PANEL = {
  ...CLOSED_PANEL,
  launch: "",
} satisfies ClustersSearch
