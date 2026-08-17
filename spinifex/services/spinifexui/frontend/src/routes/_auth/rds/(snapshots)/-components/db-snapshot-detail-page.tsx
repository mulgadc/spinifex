import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { TagsEditor } from "@/components/elbv2/tags-editor"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { formatDateTime } from "@/lib/utils"
import { useUpdateRdsTags } from "@/mutations/rds"
import {
  rdsDBSnapshotQueryOptions,
  rdsSnapshotEventsQueryOptions,
  rdsTagsQueryOptions,
} from "@/queries/rds"
import {
  canDeleteSnapshot,
  canRestoreSnapshot,
  SNAPSHOT_TYPE_AUTOMATED,
} from "@/types/rds"

import { RdsEventsPanel } from "../../-components/rds-events-panel"
import { DeleteDBSnapshotDialog } from "./delete-db-snapshot-dialog"

interface Props {
  dbSnapshotIdentifier: string
}

export function DBSnapshotDetailPage({ dbSnapshotIdentifier }: Props) {
  const navigate = useNavigate()
  const { data: snapshotData } = useSuspenseQuery(
    rdsDBSnapshotQueryOptions(dbSnapshotIdentifier),
  )
  const snapshot = snapshotData.DBSnapshots?.[0]
  const arn = snapshot?.DBSnapshotArn ?? ""

  // Not suspense: the ARN is empty until the describe lands, and an empty one
  // is a request the tags API refuses.
  const { data: tagsData } = useQuery(rdsTagsQueryOptions(arn))
  const { data: eventsData } = useSuspenseQuery(
    rdsSnapshotEventsQueryOptions(dbSnapshotIdentifier),
  )

  const updateTags = useUpdateRdsTags()
  const [showDelete, setShowDelete] = useState(false)
  const [activeTab, setActiveTab] = useState("overview")

  if (!snapshot?.DBSnapshotIdentifier) {
    return (
      <>
        <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>
        <p className="text-muted-foreground">DB snapshot not found.</p>
      </>
    )
  }

  const status = snapshot.Status
  const snapshotType = snapshot.SnapshotType
  const automated = snapshotType === SNAPSHOT_TYPE_AUTOMATED
  const sourceId = snapshot.DBInstanceIdentifier ?? ""
  const events = eventsData.Events ?? []
  const tags = tagsData?.TagList ?? []

  return (
    <>
      <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex gap-2">
              <Button
                disabled={!canRestoreSnapshot(status)}
                onClick={async () =>
                  await navigate({
                    to: "/rds/restore-db-instance-from-db-snapshot/$id",
                    params: { id: dbSnapshotIdentifier },
                  })
                }
                size="sm"
                variant="outline"
              >
                Restore
              </Button>
              <Button
                disabled={!canDeleteSnapshot(status, snapshotType)}
                onClick={() => setShowDelete(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
            </div>
          }
          subtitle="DB Snapshot"
          title={dbSnapshotIdentifier}
        />

        <div className="flex items-center gap-2">
          <StateBadge state={status} />
          {automated && (
            <span className="text-xs text-muted-foreground">
              An automated backup: it is deleted by the source instance&apos;s
              backup retention, not by hand.
            </span>
          )}
        </div>

        <Tabs onValueChange={setActiveTab} value={activeTab}>
          <TabsList>
            <TabsTab value="overview">Overview</TabsTab>
            <TabsTab value="tags">Tags</TabsTab>
            <TabsTab value="events">Events</TabsTab>
          </TabsList>

          <TabsPanel value="overview">
            <div className="space-y-4">
              <DetailCard>
                <DetailCard.Header>Snapshot</DetailCard.Header>
                <DetailCard.Content>
                  <DetailRow label="Type" value={snapshotType} />
                  <DetailRow label="Status" value={status} />
                  <DetailRow
                    label="Created"
                    value={formatDateTime(snapshot.SnapshotCreateTime)}
                  />
                  <DetailRow
                    label="Progress"
                    value={
                      snapshot.PercentProgress === undefined
                        ? undefined
                        : `${snapshot.PercentProgress}%`
                    }
                  />
                  <DetailRow label="ARN" value={snapshot.DBSnapshotArn} />
                </DetailCard.Content>
              </DetailCard>

              <DetailCard>
                <DetailCard.Header>Source</DetailCard.Header>
                <DetailCard.Content>
                  <DetailRow
                    label="DB instance"
                    value={
                      sourceId === "" ? undefined : (
                        <Link
                          className="text-primary hover:underline"
                          params={{ id: sourceId }}
                          to="/rds/describe-db-instances/$id"
                        >
                          {sourceId}
                        </Link>
                      )
                    }
                  />
                  <DetailRow label="Engine" value={snapshot.Engine} />
                  <DetailRow label="Version" value={snapshot.EngineVersion} />
                  <DetailRow
                    label="Allocated storage"
                    value={
                      snapshot.AllocatedStorage
                        ? `${snapshot.AllocatedStorage} GiB`
                        : undefined
                    }
                  />
                  <DetailRow
                    label="Storage type"
                    value={snapshot.StorageType}
                  />
                  <DetailRow
                    label="Encryption"
                    value={
                      snapshot.Encrypted
                        ? "Encrypted — always on"
                        : "Not encrypted"
                    }
                  />
                  <DetailRow
                    label="Master username"
                    value={snapshot.MasterUsername}
                  />
                  <DetailRow label="Port" value={snapshot.Port?.toString()} />
                  <DetailRow label="VPC" value={snapshot.VpcId} />
                </DetailCard.Content>
              </DetailCard>

              <p className="text-xs text-muted-foreground">
                A restore starts on this snapshot&apos;s datadir, so the engine,
                the master credentials and the initial database come with it and
                cannot be changed.
              </p>
            </div>
          </TabsPanel>

          <TabsPanel value="tags">
            <TagsEditor
              error={updateTags.error}
              isPending={updateTags.isPending}
              isSuccess={updateTags.isSuccess}
              onSubmit={(next) =>
                updateTags.mutate({
                  resourceName: arn,
                  tags: next,
                  initialKeys: tags
                    .map((t) => t.Key ?? "")
                    .filter((k) => k.length > 0),
                })
              }
              tags={tags}
            />
          </TabsPanel>

          <TabsPanel value="events">
            <RdsEventsPanel events={events} />
          </TabsPanel>
        </Tabs>
      </div>

      <DeleteDBSnapshotDialog
        dbSnapshotIdentifier={dbSnapshotIdentifier}
        onDeleted={async () =>
          await navigate({ to: "/rds/describe-db-snapshots" })
        }
        onOpenChange={setShowDelete}
        open={showDelete}
      />
    </>
  )
}
