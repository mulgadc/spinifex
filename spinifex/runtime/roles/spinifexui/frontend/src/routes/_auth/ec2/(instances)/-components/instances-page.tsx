import type { Instance } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Pause, Play, Plus, RotateCw, Search, Trash2 } from "lucide-react"
import { type ReactNode, Suspense, useState } from "react"

import { type DataColumn, DataTable } from "@/components/data-table"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailPanel, PanelSpinner } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { SelectionBar } from "@/components/selection-bar"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
} from "@/components/ui/input-group"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import {
  useRebootInstance,
  useStartInstance,
  useStopInstance,
  useTerminateInstance,
} from "@/mutations/ec2"

import { InstanceDetails } from "./instance-details"
import { RunInstancesPage } from "./run-instances-page"

export const STATE_FILTERS = ["all", "running", "stopped", "other"] as const

export type StateFilter = (typeof STATE_FILTERS)[number]

export interface InstancesSearch extends PanelSearch {
  q: string
  state: StateFilter
  sort: string
}

export function instanceName(instance: Instance): string | undefined {
  return instance.Tags?.find((tag) => tag.Key === "Name")?.Value
}

function stateOf(instance: Instance): string {
  return instance.State?.Name ?? "unknown"
}

function matchesState(instance: Instance, filter: StateFilter): boolean {
  const state = stateOf(instance)

  if (filter === "all") {
    return true
  }

  if (filter === "other") {
    return state !== "running" && state !== "stopped"
  }

  return state === filter
}

// Searches every field a customer might paste in: ID, name, type and address.
function matchesQuery(instance: Instance, query: string): boolean {
  if (!query) {
    return true
  }

  const needle = query.toLowerCase()

  return [
    instance.InstanceId,
    instanceName(instance),
    instance.InstanceType,
    instance.PrivateIpAddress,
    instance.PublicIpAddress,
    instance.ImageId,
  ].some((value) => value?.toLowerCase().includes(needle))
}

function Mono({ children }: { children: string | undefined }) {
  return children ? (
    <span className="font-mono text-xs tabular-nums">{children}</span>
  ) : (
    <span className="text-muted-foreground">—</span>
  )
}

const COLUMNS: DataColumn<Instance>[] = [
  {
    id: "instance",
    header: "Instance",
    sortValue: (instance) => instanceName(instance) ?? instance.InstanceId,
    cell: (instance) => {
      const name = instanceName(instance)
      const id = instance.InstanceId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-instances/"
          search={(previous) => ({
            ...previous,
            ...CLOSED_PANEL,
            selected: id,
          })}
          to="."
        >
          <span className="block truncate font-medium text-primary group-hover:underline">
            {name ?? id}
          </span>
          {name && (
            <span className="block truncate font-mono text-xs text-muted-foreground">
              {id}
            </span>
          )}
        </Link>
      )
    },
  },
  {
    id: "state",
    header: "State",
    sortValue: stateOf,
    cell: (instance) => <StateBadge state={instance.State?.Name} />,
  },
  {
    id: "type",
    header: "Type",
    sortValue: (instance) => instance.InstanceType,
    hideBelow: "sm",
    cell: (instance) => <Mono>{instance.InstanceType}</Mono>,
  },
  {
    id: "az",
    header: "Zone",
    sortValue: (instance) => instance.Placement?.AvailabilityZone,
    hideBelow: "lg",
    cell: (instance) => instance.Placement?.AvailabilityZone ?? "—",
  },
  {
    id: "private-ip",
    header: "Private IP",
    sortValue: (instance) => instance.PrivateIpAddress,
    hideBelow: "md",
    cell: (instance) => <Mono>{instance.PrivateIpAddress}</Mono>,
  },
  {
    id: "public-ip",
    header: "Public IP",
    sortValue: (instance) => instance.PublicIpAddress,
    hideBelow: "md",
    cell: (instance) => <Mono>{instance.PublicIpAddress}</Mono>,
  },
  {
    id: "launched",
    header: "Launched",
    sortValue: (instance) =>
      instance.LaunchTime ? new Date(instance.LaunchTime).getTime() : undefined,
    hideBelow: "xl",
    cell: (instance) => (
      <span className="text-muted-foreground">
        {formatDateTime(instance.LaunchTime)}
      </span>
    ),
  },
]

// "-launched" sorts launched descending; an unknown column falls back to none.
export function parseSort(sort: string) {
  const desc = sort.startsWith("-")
  const column = desc ? sort.slice(1) : sort

  return COLUMNS.some((candidate) => candidate.id === column)
    ? { column, desc }
    : undefined
}

const ACTION_KEYS = ["start", "stop", "reboot"] as const

const ACTIONS = {
  start: { label: "Start", icon: Play, from: "stopped" },
  stop: { label: "Stop", icon: Pause, from: "running" },
  reboot: { label: "Reboot", icon: RotateCw, from: "running" },
} as const

interface BulkActionsProps {
  instances: readonly Instance[]
  onDone: () => void
}

// Each action applies only to the selected instances it can act on, and says
// how many that is, so a mixed selection never sends a call that must fail.
function BulkActions({ instances, onDone }: BulkActionsProps) {
  const mutations = {
    start: useStartInstance(),
    stop: useStopInstance(),
    reboot: useRebootInstance(),
  }

  const terminate = useTerminateInstance()
  const [confirmTerminate, setConfirmTerminate] = useState(false)

  const terminable = instances.flatMap((instance) =>
    instance.InstanceId &&
    !["terminated", "shutting-down"].includes(stateOf(instance))
      ? [instance.InstanceId]
      : [],
  )

  const error = [...Object.values(mutations), terminate].find(
    (mutation) => mutation.error,
  )?.error

  return (
    <SelectionBar
      count={instances.length}
      footer={error && <ErrorBanner error={error} />}
      onClear={onDone}
    >
      {ACTION_KEYS.map((key) => {
        const action = ACTIONS[key]
        const mutation = mutations[key]

        const ids = instances.flatMap((instance) =>
          instance.InstanceId && stateOf(instance) === action.from
            ? [instance.InstanceId]
            : [],
        )

        return (
          <Button
            disabled={ids.length === 0 || mutation.isPending}
            key={key}
            onClick={() => {
              mutation.mutate(ids, { onSuccess: onDone })
            }}
            size="sm"
            variant="outline"
          >
            <action.icon data-icon="inline-start" />
            {action.label}
            {ids.length > 0 && ids.length < instances.length && (
              <span className="text-muted-foreground tabular-nums">
                {ids.length}
              </span>
            )}
          </Button>
        )
      })}
      <Button
        disabled={terminable.length === 0}
        onClick={() => {
          setConfirmTerminate(true)
        }}
        size="sm"
        variant="destructive"
      >
        <Trash2 data-icon="inline-start" />
        Terminate
      </Button>
      <DeleteConfirmationDialog
        confirmLabel={`Terminate ${terminable.length}`}
        description={
          <>
            Terminating deletes the instance and any volume set to delete on
            termination. This cannot be undone.
            <span className="mt-2 block font-mono text-xs break-all">
              {terminable.join(", ")}
            </span>
          </>
        }
        isPending={terminate.isPending}
        onConfirm={() => {
          terminate.mutate(terminable, {
            onSettled: () => {
              setConfirmTerminate(false)
            },
            onSuccess: onDone,
          })
        }}
        onOpenChange={setConfirmTerminate}
        open={confirmTerminate}
        confirmPhrase="terminate"
        pendingLabel="Terminating…"
        title={`Terminate ${terminable.length} ${terminable.length === 1 ? "instance" : "instances"}?`}
      />
    </SelectionBar>
  )
}

interface InstancesPageProps {
  instances: readonly Instance[]
  search: InstancesSearch
  onSearchChange: (search: Partial<InstancesSearch>) => void
}

export function InstancesPage({
  instances,
  search,
  onSearchChange,
}: InstancesPageProps) {
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const sort = parseSort(search.sort)

  const counts = Object.fromEntries(
    STATE_FILTERS.map((filter) => [
      filter,
      instances.filter((instance) => matchesState(instance, filter)).length,
    ]),
  )

  const visible = instances.filter(
    (instance) =>
      matchesState(instance, search.state) && matchesQuery(instance, search.q),
  )

  // Selection is pruned to what the filters show, so actions never reach a
  // row the customer can no longer see.
  const selectedVisible = visible.filter(
    (instance) => instance.InstanceId && selected.has(instance.InstanceId),
  )

  const filtered = search.q !== "" || search.state !== "all"

  const open = instances.find(
    (instance) =>
      search.selected !== "" && instance.InstanceId === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <DetailPanel
        closeLabel="Close launch form"
        label="Launch instance"
        onClose={closePanel}
        size="wide"
        title="Launch instance"
      >
        <Suspense fallback={<PanelSpinner />}>
          <RunInstancesPage onClose={closePanel} />
        </Suspense>
      </DetailPanel>
    )
  } else if (open) {
    panel = (
      <InstanceDetails
        instance={open}
        key={open.InstanceId}
        name={instanceName(open)}
        onClose={closePanel}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        tab={search.tab}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/ec2/describe-instances/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Launch instance
          </Link>
        }
        subtitle="Virtual machines running on this cluster."
        title="Instances"
      />

      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <InputGroup className="w-full sm:w-72">
            <InputGroupAddon>
              <Search />
            </InputGroupAddon>
            <InputGroupInput
              aria-label="Filter instances"
              onChange={(event) => {
                onSearchChange({ q: event.target.value })
              }}
              placeholder="Filter by name, ID, type or IP"
              type="search"
              value={search.q}
            />
          </InputGroup>
          <ToggleGroup
            aria-label="Instance state"
            onValueChange={(values) => {
              const next = STATE_FILTERS.find(
                (filter) => filter === values.at(-1),
              )

              if (next) {
                onSearchChange({ state: next })
              }
            }}
            value={[search.state]}
            variant="outline"
          >
            {STATE_FILTERS.map((filter) => (
              <ToggleGroupItem key={filter} value={filter}>
                <span className="capitalize">{filter}</span>
                <span className="text-muted-foreground tabular-nums">
                  {counts[filter]}
                </span>
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
          {filtered && (
            <Button
              onClick={() => {
                onSearchChange({ q: "", state: "all" })
              }}
              size="sm"
              variant="ghost"
            >
              Clear filters
            </Button>
          )}
        </div>

        <div>
          {selectedVisible.length > 0 && (
            <BulkActions
              instances={selectedVisible}
              onDone={() => {
                setSelected(new Set())
              }}
            />
          )}

          <DataTable
            caption="Instances"
            columns={COLUMNS}
            empty={
              filtered ? (
                "No instances match these filters."
              ) : (
                <span className="flex flex-col items-center gap-3">
                  No instances yet.
                  <Link
                    className={buttonVariants({
                      size: "sm",
                      variant: "outline",
                    })}
                    to="/ec2/run-instances"
                  >
                    Launch your first instance
                  </Link>
                </span>
              )
            }
            activeId={open?.InstanceId}
            getRowId={(instance) => instance.InstanceId ?? ""}
            onRowClick={(id) => {
              onSearchChange({ ...CLOSED_PANEL, selected: id })
            }}
            onSelectedChange={(next) => {
              setSelected(next)

              // The bulk bar and the panel compete for the same attention.
              if (next.size > 0 && search.selected !== "") {
                closePanel()
              }
            }}
            onSortChange={(next) => {
              onSearchChange({
                sort: `${next.desc ? "-" : ""}${next.column}`,
              })
            }}
            rows={visible}
            selected={selected}
            sort={sort}
          />
        </div>

        <p className="text-xs text-muted-foreground tabular-nums">
          {filtered
            ? `${visible.length} of ${instances.length} instances`
            : `${instances.length} ${instances.length === 1 ? "instance" : "instances"}`}
        </p>
      </div>
    </ResourceView>
  )
}
