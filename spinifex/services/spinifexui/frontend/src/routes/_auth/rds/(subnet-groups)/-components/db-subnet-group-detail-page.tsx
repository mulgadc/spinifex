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
import { useUpdateRdsTags } from "@/mutations/rds"
import { rdsSubnetGroupQueryOptions, rdsTagsQueryOptions } from "@/queries/rds"

import { DeleteDBSubnetGroupDialog } from "./delete-db-subnet-group-dialog"

interface Props {
  dbSubnetGroupName: string
}

export function DBSubnetGroupDetailPage({ dbSubnetGroupName }: Props) {
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(
    rdsSubnetGroupQueryOptions(dbSubnetGroupName),
  )
  const group = data.DBSubnetGroups?.[0]
  const arn = group?.DBSubnetGroupArn ?? ""

  // Not suspense: the ARN is empty for a resource that has none, and an empty
  // one is a request the tags API refuses.
  const { data: tagsData } = useQuery(rdsTagsQueryOptions(arn))
  const updateTags = useUpdateRdsTags()
  const [showDelete, setShowDelete] = useState(false)
  const [activeTab, setActiveTab] = useState("subnets")

  if (!group?.DBSubnetGroupName) {
    return (
      <>
        <BackLink to="/rds/describe-db-subnet-groups">
          Back to subnet groups
        </BackLink>
        <p className="text-muted-foreground">DB subnet group not found.</p>
      </>
    )
  }

  const subnets = group.Subnets ?? []
  const tags = tagsData?.TagList ?? []

  return (
    <>
      <BackLink to="/rds/describe-db-subnet-groups">
        Back to subnet groups
      </BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={
            <Button
              onClick={() => setShowDelete(true)}
              size="sm"
              variant="destructive"
            >
              <Trash2 className="size-4" />
              Delete
            </Button>
          }
          subtitle="DB Subnet Group"
          title={dbSubnetGroupName}
        />

        <div className="flex items-center gap-2">
          <StateBadge state={group.SubnetGroupStatus} />
        </div>

        <Tabs onValueChange={setActiveTab} value={activeTab}>
          <TabsList>
            <TabsTab value="subnets">Subnets</TabsTab>
            <TabsTab value="tags">Tags</TabsTab>
          </TabsList>

          <TabsPanel value="subnets">
            <div className="space-y-4">
              <DetailCard>
                <DetailCard.Header>Details</DetailCard.Header>
                <DetailCard.Content>
                  <DetailRow
                    label="Description"
                    value={group.DBSubnetGroupDescription}
                  />
                  <DetailRow label="VPC" value={group.VpcId} />
                  <DetailRow
                    label="Subnets"
                    value={subnets.length.toString()}
                  />
                  <DetailRow label="ARN" value={group.DBSubnetGroupArn} />
                </DetailCard.Content>
              </DetailCard>

              {subnets.length > 0 ? (
                <div className="overflow-x-auto rounded-lg border bg-card">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b text-left text-muted-foreground">
                        <th className="px-4 py-2 font-medium">Subnet</th>
                        <th className="px-4 py-2 font-medium">
                          Availability zone
                        </th>
                        <th className="px-4 py-2 font-medium">Status</th>
                      </tr>
                    </thead>
                    <tbody>
                      {subnets.map((subnet) => (
                        <tr
                          className="border-b last:border-0"
                          key={subnet.SubnetIdentifier}
                        >
                          <td className="px-4 py-2 font-mono text-xs">
                            <Link
                              className="text-primary hover:underline"
                              params={{ id: subnet.SubnetIdentifier ?? "" }}
                              to="/ec2/describe-subnets/$id"
                            >
                              {subnet.SubnetIdentifier}
                            </Link>
                          </td>
                          <td className="px-4 py-2">
                            {subnet.SubnetAvailabilityZone?.Name ?? "—"}
                          </td>
                          <td className="px-4 py-2">{subnet.SubnetStatus}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : (
                <p className="text-muted-foreground">
                  This group holds no subnets.
                </p>
              )}

              <p className="text-xs text-muted-foreground">
                This group records which subnets RDS may place an endpoint in;
                each subnet&apos;s own CIDR, addresses and routing live on its
                EC2 page. Membership is fixed at create — ModifyDBSubnetGroup is
                not implemented. Changing it means creating a new group, which a
                DB instance can only adopt by being recreated.
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
        </Tabs>
      </div>

      <DeleteDBSubnetGroupDialog
        dbSubnetGroupName={dbSubnetGroupName}
        onDeleted={async () =>
          await navigate({ to: "/rds/describe-db-subnet-groups" })
        }
        onOpenChange={setShowDelete}
        open={showDelete}
      />
    </>
  )
}
