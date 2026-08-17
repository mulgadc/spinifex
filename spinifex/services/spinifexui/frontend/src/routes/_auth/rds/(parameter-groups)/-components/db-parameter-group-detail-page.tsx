import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { TagsEditor } from "@/components/elbv2/tags-editor"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { useUpdateRdsTags } from "@/mutations/rds"
import {
  rdsParameterGroupQueryOptions,
  rdsParametersQueryOptions,
  rdsTagsQueryOptions,
} from "@/queries/rds"
import { isDefaultParameterGroupName } from "@/types/rds"

import { DeleteDBParameterGroupDialog } from "./delete-db-parameter-group-dialog"
import { ParametersEditor } from "./parameters-editor"

interface Props {
  dbParameterGroupName: string
}

export function DBParameterGroupDetailPage({ dbParameterGroupName }: Props) {
  const navigate = useNavigate()
  const { data: groupData } = useSuspenseQuery(
    rdsParameterGroupQueryOptions(dbParameterGroupName),
  )
  const group = groupData.DBParameterGroups?.[0]
  const isDefault = isDefaultParameterGroupName(dbParameterGroupName)
  // A default group has no stored record, so ListTagsForResource has nothing
  // to answer for it and the tags tab is not offered.
  const arn = isDefault ? "" : (group?.DBParameterGroupArn ?? "")

  const { data: parametersData } = useSuspenseQuery(
    rdsParametersQueryOptions(dbParameterGroupName),
  )
  // Not suspense: the ARN is empty for a resource that has none, and an empty
  // one is a request the tags API refuses.
  const { data: tagsData } = useQuery(rdsTagsQueryOptions(arn))

  const updateTags = useUpdateRdsTags()
  const [showDelete, setShowDelete] = useState(false)
  const [activeTab, setActiveTab] = useState("parameters")

  if (!group?.DBParameterGroupName) {
    return (
      <>
        <BackLink to="/rds/describe-db-parameter-groups">
          Back to parameter groups
        </BackLink>
        <p className="text-muted-foreground">DB parameter group not found.</p>
      </>
    )
  }

  const parameters = parametersData.Parameters ?? []
  const tags = tagsData?.TagList ?? []
  const overrideCount = parameters.filter((p) => p.Source === "user").length

  return (
    <>
      <BackLink to="/rds/describe-db-parameter-groups">
        Back to parameter groups
      </BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={
            <Button
              disabled={isDefault}
              onClick={() => setShowDelete(true)}
              size="sm"
              variant="destructive"
            >
              <Trash2 className="size-4" />
              Delete
            </Button>
          }
          subtitle="DB Parameter Group"
          title={dbParameterGroupName}
        />

        {isDefault && (
          <div
            className="rounded-md border border-tactical-amber/40 bg-tactical-amber/5 p-4 text-sm"
            role="note"
          >
            <p className="font-medium">This is a default parameter group</p>
            <p className="mt-1 text-xs text-muted-foreground">
              The service owns it. It can be read and attached to an instance,
              but it cannot be modified, deleted or tagged. Create your own
              group of the same family to change a value.
            </p>
          </div>
        )}

        <Tabs onValueChange={setActiveTab} value={activeTab}>
          <TabsList>
            <TabsTab value="parameters">Parameters</TabsTab>
            <TabsTab value="details">Details</TabsTab>
            {!isDefault && <TabsTab value="tags">Tags</TabsTab>}
          </TabsList>

          <TabsPanel value="parameters">
            <ParametersEditor
              dbParameterGroupName={dbParameterGroupName}
              parameters={parameters}
              readOnly={isDefault}
            />
          </TabsPanel>

          <TabsPanel value="details">
            <DetailCard>
              <DetailCard.Header>Details</DetailCard.Header>
              <DetailCard.Content>
                <DetailRow
                  label="Family"
                  value={group.DBParameterGroupFamily}
                />
                <DetailRow label="Description" value={group.Description} />
                <DetailRow
                  label="Type"
                  value={isDefault ? "Default" : "Customer"}
                />
                <DetailRow
                  label="Parameters"
                  value={parameters.length.toString()}
                />
                <DetailRow
                  label="Overridden"
                  value={overrideCount.toString()}
                />
                <DetailRow label="ARN" value={group.DBParameterGroupArn} />
              </DetailCard.Content>
            </DetailCard>
          </TabsPanel>

          {!isDefault && (
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
          )}
        </Tabs>
      </div>

      <DeleteDBParameterGroupDialog
        dbParameterGroupName={dbParameterGroupName}
        onDeleted={async () =>
          await navigate({ to: "/rds/describe-db-parameter-groups" })
        }
        onOpenChange={setShowDelete}
        open={showDelete}
      />
    </>
  )
}
