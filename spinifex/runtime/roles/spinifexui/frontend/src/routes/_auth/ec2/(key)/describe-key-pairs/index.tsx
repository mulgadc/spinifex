import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { panelSearchFields } from "@/lib/panel-search"
import { ec2KeyPairsQueryOptions } from "@/queries/ec2"

import {
  CLOSED_KEY_PAIRS_PANEL,
  type KeyPairsSearch,
  KeyPairsPage,
} from "../-components/key-pairs-page"

// Key pairs have two create forms, so importing has its own flag.
/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({
  ...panelSearchFields,
  import: z.boolean().default(false).catch(false),
})
/* oxlint-enable promise/prefer-await-to-then */

export const Route = createFileRoute("/_auth/ec2/(key)/describe-key-pairs/")({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_KEY_PAIRS_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2KeyPairsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Key Pairs | EC2 | Mulga",
      },
    ],
  }),
  component: KeyPairs,
})

function KeyPairs() {
  const { data } = useSuspenseQuery(ec2KeyPairsQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<KeyPairsSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next || "import" in next),
    })
  }

  return (
    <KeyPairsPage
      keyPairs={data.KeyPairs ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
