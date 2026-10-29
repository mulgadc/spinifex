import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { useState } from "react"

import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  useAssociateIamInstanceProfile,
  useDisassociateIamInstanceProfile,
} from "@/mutations/ec2"
import { ec2IamInstanceProfileAssociationsQueryOptions } from "@/queries/ec2"
import { iamInstanceProfilesQueryOptions } from "@/queries/iam"

export function IamRolePanel({ instanceId }: { instanceId: string }) {
  const { data: associationsData } = useQuery(
    ec2IamInstanceProfileAssociationsQueryOptions(instanceId),
  )

  const { data: profilesData } = useQuery(iamInstanceProfilesQueryOptions)

  const associateMutation = useAssociateIamInstanceProfile()
  const disassociateMutation = useDisassociateIamInstanceProfile()

  const [selectedProfile, setSelectedProfile] = useState("")

  // The API can return stale "disassociated" associations; only an active one
  // actually flows credentials to the instance's IMDS.
  const association = associationsData?.IamInstanceProfileAssociations?.find(
    (a) => a.State === "associating" || a.State === "associated",
  )

  const profiles = profilesData?.InstanceProfiles ?? []

  const handleAssociate = async () => {
    if (!selectedProfile) {
      return
    }

    await associateMutation.mutateAsync({
      instanceId,
      instanceProfileName: selectedProfile,
    })
    setSelectedProfile("")
  }

  const handleDisassociate = async () => {
    if (!association?.AssociationId) {
      return
    }

    await disassociateMutation.mutateAsync({
      associationId: association.AssociationId,
      instanceId,
    })
  }

  return (
    <PanelSection title="IAM role">
      <div className="flex flex-col gap-3">
        {associateMutation.error && (
          <ErrorBanner
            error={associateMutation.error}
            msg="Failed to associate instance profile"
          />
        )}
        {disassociateMutation.error && (
          <ErrorBanner
            error={disassociateMutation.error}
            msg="Failed to disassociate instance profile"
          />
        )}

        {association ? (
          <>
            <PanelFields>
              <PanelField
                copy={association.IamInstanceProfile?.Arn}
                label="Instance profile"
                mono
              >
                {association.IamInstanceProfile?.Arn}
              </PanelField>
              <PanelField label="State">
                <StateBadge state={association.State} />
              </PanelField>
            </PanelFields>
            <Button
              className="self-start"
              disabled={disassociateMutation.isPending}
              onClick={handleDisassociate}
              size="sm"
              variant="destructive"
            >
              {disassociateMutation.isPending ? "Detaching…" : "Detach"}
            </Button>
          </>
        ) : (
          <>
            <p className="text-sm text-muted-foreground">
              No instance profile is associated. Associate one to flow role
              credentials to this instance via IMDS.
            </p>
            {profiles.length > 0 ? (
              <div className="flex items-center gap-2">
                <Select
                  onValueChange={(value) => {
                    setSelectedProfile(value ?? "")
                  }}
                  value={selectedProfile}
                >
                  <SelectTrigger
                    aria-label="Instance profile"
                    className="w-full"
                  >
                    <SelectValue placeholder="Select an instance profile" />
                  </SelectTrigger>
                  <SelectContent>
                    {profiles.map((profile) => (
                      <SelectItem
                        key={profile.InstanceProfileName}
                        value={profile.InstanceProfileName ?? ""}
                      >
                        {profile.InstanceProfileName}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <Button
                  disabled={!selectedProfile || associateMutation.isPending}
                  onClick={handleAssociate}
                  size="sm"
                >
                  {associateMutation.isPending ? "Associating…" : "Associate"}
                </Button>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">
                No instance profiles exist.{" "}
                <Link
                  className="text-primary hover:underline"
                  to="/iam/create-instance-profile"
                >
                  Create one
                </Link>{" "}
                first.
              </p>
            )}
          </>
        )}
      </div>
    </PanelSection>
  )
}
