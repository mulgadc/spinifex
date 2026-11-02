import { Plus, Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { ConfirmPhrase } from "@/components/confirm-phrase"
import { ConsoleOutput } from "@/components/console-output"
import { type DataColumn, DataTable } from "@/components/data-table"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { InfoList, InfoTip } from "@/components/info-tip"
import { PageHeading } from "@/components/page-heading"
import { PickerNoticeText } from "@/components/picker-notice"
import { SelectionBar } from "@/components/selection-bar"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { WizardFailureBanner } from "@/components/wizard-failure-banner"

import { CatalogueSection, Specimen, unwired } from "./catalogue-section"

interface SampleVolume {
  id: string
  size: number
  state: string
}

const VOLUMES: SampleVolume[] = [
  { id: "vol-0a1b2c3d4e5f60001", size: 40, state: "in-use" },
  { id: "vol-0a1b2c3d4e5f60002", size: 8, state: "available" },
  { id: "vol-0a1b2c3d4e5f60003", size: 100, state: "creating" },
]

const VOLUME_COLUMNS: DataColumn<SampleVolume>[] = [
  {
    id: "id",
    header: "Volume ID",
    cell: (row) => <span className="font-mono">{row.id}</span>,
    sortValue: (row) => row.id,
  },
  {
    id: "size",
    header: "Size",
    cell: (row) => `${row.size} GiB`,
    sortValue: (row) => row.size,
    align: "end",
  },
  {
    id: "state",
    header: "State",
    cell: (row) => <StateBadge state={row.state} />,
    hideBelow: "sm",
  },
]

const BOOT_LOG = `[    0.000000] Linux version 6.12.0 (builder@spinifex)
[    1.204118] virtio_blk virtio2: [vda] 41943040 512-byte logical blocks
[    3.880021] cloud-init[512]: Cloud-init v. 25.1 finished`

const REFUSED = Object.assign(
  new Error("The subnet CIDR overlaps 10.0.1.0/24."),
  { name: "InvalidSubnet.Conflict" },
)

export function PatternsSection() {
  const [phrase, setPhrase] = useState("")
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [deleting, setDeleting] = useState(false)

  return (
    <CatalogueSection
      description="The shared pieces every list, panel and page is assembled from."
      title="Patterns"
    >
      <Specimen label="Page heading">
        <div className="w-full">
          <PageHeading
            actions={
              <Button>
                <Plus data-icon="inline-start" />
                Create VPC
              </Button>
            }
            subtitle="Isolated networks for your instances."
            title="VPCs"
          />
        </div>
      </Specimen>
      <Specimen label="Back link">
        <BackLink to="/design">Back to the catalogue</BackLink>
      </Specimen>
      <Specimen label="Selection bar">
        <div className="w-full">
          <SelectionBar count={3} onClear={unwired}>
            <Button size="sm" variant="destructive">
              <Trash2 data-icon="inline-start" />
              Delete
            </Button>
          </SelectionBar>
        </div>
      </Specimen>
      <Specimen label="Data table: sort, select, click a row to open its panel">
        <div className="w-full">
          <DataTable
            caption="Sample volumes"
            columns={VOLUME_COLUMNS}
            empty="No volumes in this region."
            getRowId={(row) => row.id}
            onSelectedChange={setSelected}
            rows={VOLUMES}
            selected={selected}
          />
        </div>
      </Specimen>
      <Specimen label="Irreversible delete, typed confirmation">
        <Button
          onClick={() => {
            setDeleting(true)
          }}
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete volume
        </Button>
        <DeleteConfirmationDialog
          confirmPhrase="delete"
          description="vol-0a1b2c3d4e5f60002 and its data are removed. This cannot be undone."
          isPending={false}
          onConfirm={() => {
            setDeleting(false)
          }}
          onOpenChange={setDeleting}
          open={deleting}
          title="Delete volume?"
        />
      </Specimen>
      <Specimen label="Console output">
        <div className="w-full">
          <ConsoleOutput output={BOOT_LOG} />
        </div>
      </Specimen>
      <Specimen label="Panel section and fields">
        <div className="w-full max-w-xl">
          <PanelSection title="Instance information">
            <PanelFields>
              <PanelField copy="i-0a1b2c3d4e5f67890" label="Instance ID" mono>
                i-0a1b2c3d4e5f67890
              </PanelField>
              <PanelField label="State">
                <StateBadge state="running" />
              </PanelField>
              <PanelField
                info={
                  <InfoList
                    items={[
                      ["Distro", "Ubuntu 26.04"],
                      ["Boot mode", "uefi"],
                    ]}
                  />
                }
                label="Login user"
                mono
              >
                ubuntu
              </PanelField>
            </PanelFields>
          </PanelSection>
        </div>
      </Specimen>
      <Specimen label="Info tip">
        <span className="flex items-center gap-1">
          Tenancy
          <InfoTip label="About tenancy">
            Dedicated runs the instance on hardware no other account uses.
          </InfoTip>
        </span>
      </Specimen>
      <Specimen label="Errors">
        <div className="flex w-full flex-col gap-3">
          <ErrorBanner
            className="mb-0"
            error={REFUSED}
            msg="Failed to create subnet"
          />
          <WizardFailureBanner
            result={{
              created: [{ type: "VPC", id: "vpc-0a1b2c3d" }],
              error: REFUSED,
              failedStep: "Failed while creating public subnets",
            }}
          />
        </div>
      </Specimen>
      <Specimen label="Picker notices">
        <PickerNoticeText
          notice={{ tone: "muted", text: "Loading subnets…" }}
        />
        <PickerNoticeText
          notice={{ tone: "error", text: "Could not load subnets." }}
        />
      </Specimen>
      <Specimen label="Typed confirmation">
        <div className="w-full max-w-md">
          <ConfirmPhrase onChange={setPhrase} phrase="delete" value={phrase} />
        </div>
      </Specimen>
    </CatalogueSection>
  )
}
