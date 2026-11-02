import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2KeyPairQueryOptions } from "@/queries/ec2"

import { KeyPairDetails } from "../-components/key-pair-details"

export const Route = createFileRoute("/_auth/ec2/(key)/describe-key-pairs/$id")(
  {
    validateSearch: z.object({ tab: panelSearchFields.tab }),
    search: { middlewares: [stripSearchParams({ tab: "overview" })] },
    loader: async ({ context, params }) =>
      await context.queryClient.query({
        ...ec2KeyPairQueryOptions(params.id),
        staleTime: "static",
      }),
    head: ({ loaderData }) => ({
      meta: [
        {
          title: `${loaderData?.KeyPairs?.[0]?.KeyName ?? "Key Pair"} | EC2 | Mulga`,
        },
      ],
    }),
    component: KeyPairDetail,
  },
)

// The key pair's own page: the side panel's tabs, laid out for the full width.
function KeyPairDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2KeyPairQueryOptions(id))
  const keyPair = data.KeyPairs?.[0]

  if (!keyPair?.KeyPairId) {
    return (
      <>
        <BackLink to="/ec2/describe-key-pairs">Back to key pairs</BackLink>
        <p className="text-muted-foreground">Key pair not found.</p>
      </>
    )
  }

  return (
    <KeyPairDetails
      keyPair={keyPair}
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-key-pairs" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
