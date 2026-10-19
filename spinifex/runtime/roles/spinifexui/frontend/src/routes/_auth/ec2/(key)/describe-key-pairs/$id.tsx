import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { DeleteResourceButton } from "@/components/delete-resource-button"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { formatDateTime } from "@/lib/utils"
import { useDeleteKeyPair } from "@/mutations/ec2"
import { ec2KeyPairQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(key)/describe-key-pairs/$id")(
  {
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

function KeyPairDetail() {
  const { id } = Route.useParams()
  const { data } = useSuspenseQuery(ec2KeyPairQueryOptions(id))
  const keyPair = data.KeyPairs?.[0]
  const deleteMutation = useDeleteKeyPair()

  if (!keyPair?.KeyPairId) {
    return (
      <>
        <BackLink to="/ec2/describe-key-pairs">Back to key pairs</BackLink>
        <p className="text-muted-foreground">Key pair not found.</p>
      </>
    )
  }

  const createTime = formatDateTime(keyPair.CreateTime)

  return (
    <>
      <BackLink to="/ec2/describe-key-pairs">Back to key pairs</BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={
            <DeleteResourceButton
              description={`Are you sure you want to delete the key pair "${keyPair.KeyName}"? This action cannot be undone. Any instances using this key pair will no longer be able to be accessed with it.`}
              id={id}
              mutation={deleteMutation}
              title="Delete Key Pair"
              to="/ec2/describe-key-pairs"
            />
          }
          subtitle="Key Pair Details"
          title={keyPair.KeyName ?? ""}
        />

        {/* Key Pair Details */}
        <DetailCard>
          <DetailCard.Header>Key Pair Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Key Pair ID" value={keyPair.KeyPairId} />
            <DetailRow label="Key Name" value={keyPair.KeyName} />
            <DetailRow label="Key Type" value={keyPair.KeyType} />
            <DetailRow label="Key Fingerprint" value={keyPair.KeyFingerprint} />
            <DetailRow label="Creation Date" value={createTime} />
          </DetailCard.Content>
        </DetailCard>

        {/* Tags */}
        {keyPair.Tags && keyPair.Tags.length > 0 && (
          <DetailCard>
            <DetailCard.Header>Tags</DetailCard.Header>
            <DetailCard.Content>
              {keyPair.Tags.map((tag) => (
                <DetailRow
                  key={tag.Key}
                  label={tag.Key ?? ""}
                  value={tag.Value}
                />
              ))}
            </DetailCard.Content>
          </DetailCard>
        )}
      </div>
    </>
  )
}
