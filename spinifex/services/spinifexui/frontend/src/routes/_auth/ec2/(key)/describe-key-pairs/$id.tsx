import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { useDeleteKeyPair } from "@/mutations/ec2"
import { ec2KeyPairQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(key)/describe-key-pairs/$id")(
  {
    loader: async ({ context, params }) =>
      await context.queryClient.ensureQueryData(
        ec2KeyPairQueryOptions(params.id),
      ),
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
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2KeyPairQueryOptions(id))
  const keyPair = data.KeyPairs?.[0]
  const deleteMutation = useDeleteKeyPair()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(id)
      navigate({ to: "/ec2/describe-key-pairs" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

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

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete key pair"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <Button
              onClick={() => setShowDeleteDialog(true)}
              size="sm"
              variant="destructive"
            >
              <Trash2 className="size-4" />
              Delete
            </Button>
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

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the key pair "${keyPair.KeyName}"? This action cannot be undone. Any instances using this key pair will no longer be able to be accessed with it.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Key Pair"
      />
    </>
  )
}
