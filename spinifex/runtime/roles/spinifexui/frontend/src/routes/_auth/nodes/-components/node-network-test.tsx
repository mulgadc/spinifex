import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Gauge } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { SpeedDial } from "@/components/speed-dial"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { formatLinkSpeed, PLANE_LABELS, PLANE_ORDER } from "@/lib/node-topology"
import {
  type NetworkTest,
  type NetworkTestRun,
  networkTestsQueryOptions,
  type NodeFacts,
  type Plane,
  startNetworkTest,
} from "@/queries/nodes"

const ALL = "__all__"
const DURATIONS = ["10", "20", "30"]
const STREAMS = ["1", "4", "8"]

interface NodeNetworkTestProps {
  node: string
  facts: NodeFacts | undefined
  peers: readonly string[]
}

function Choice({
  id,
  label,
  value,
  onChange,
  options,
}: {
  id: string
  label: string
  value: string
  onChange: (value: string) => void
  options: readonly { value: string; label: string }[]
}) {
  return (
    <Field>
      <FieldTitle>
        <label htmlFor={id}>{label}</label>
      </FieldTitle>
      <Select
        onValueChange={(next) => {
          onChange(next ?? value)
        }}
        value={value}
      >
        <SelectTrigger className="w-full" id={id}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {options.map((option) => (
            <SelectItem key={option.value} value={option.value}>
              {option.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  )
}

// Runs iperf between this node and its peers over one plane. Both directions
// are measured, one after the other, so neither run shares the link.
export function NodeNetworkTest({ node, facts, peers }: NodeNetworkTestProps) {
  const queryClient = useQueryClient()

  const planes = PLANE_ORDER.filter((plane) =>
    (facts?.planes ?? []).some((p) => p.plane === plane && p.address),
  )

  const [plane, setPlane] = useState<Plane>(
    planes.includes("lan") ? "lan" : (planes[0] ?? "lan"),
  )

  const [target, setTarget] = useState(ALL)
  const [duration, setDuration] = useState("10")
  const [streams, setStreams] = useState("4")
  const [confirmWAN, setConfirmWAN] = useState(false)

  const { data } = useQuery(networkTestsQueryOptions)

  const start = useMutation({
    mutationFn: startNetworkTest,
    onSuccess: async () => {
      setConfirmWAN(false)
      await queryClient.invalidateQueries({
        queryKey: networkTestsQueryOptions.queryKey,
      })
    },
  })

  const running = data?.tests.find((test) => test.status === "running")

  const latest = data?.tests.find((test) =>
    test.runs.some((run) => run.from === node || run.to === node),
  )

  function startTest() {
    start.mutate({
      plane,
      source: node,
      ...(target !== ALL && { target }),
      durationSeconds: Number(duration),
      streams: Number(streams),
    })
  }

  const others = peers.filter((peer) => peer !== node)

  return (
    <>
      <PanelSection title="Network test">
        {others.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            A network test needs a second node to send to.
          </p>
        ) : (
          <div className="flex flex-col gap-3">
            <div className="grid grid-cols-2 gap-3">
              <Choice
                id="nettest-plane"
                label="Plane"
                onChange={(value) => {
                  setPlane(planes.find((p) => p === value) ?? plane)
                }}
                options={planes.map((p) => ({
                  value: p,
                  label: PLANE_LABELS[p],
                }))}
                value={plane}
              />
              <Choice
                id="nettest-target"
                label="Peer"
                onChange={setTarget}
                options={[
                  { value: ALL, label: "Every node (full mesh)" },
                  ...others.map((peer) => ({ value: peer, label: peer })),
                ]}
                value={target}
              />
              <Choice
                id="nettest-duration"
                label="Duration per direction"
                onChange={setDuration}
                options={DURATIONS.map((d) => ({
                  value: d,
                  label: `${d} seconds`,
                }))}
                value={duration}
              />
              <Choice
                id="nettest-streams"
                label="Parallel streams"
                onChange={setStreams}
                options={STREAMS.map((s) => ({ value: s, label: s }))}
                value={streams}
              />
            </div>
            <p className="text-2xs text-muted-foreground">
              The test saturates the link for its duration, so guests on these
              nodes see less bandwidth while it runs.
            </p>
            {start.isError && (
              <ErrorBanner error={start.error} msg="The test did not start." />
            )}
            <div>
              <Button
                disabled={
                  start.isPending ||
                  running !== undefined ||
                  planes.length === 0
                }
                onClick={() => {
                  if (plane === "wan") {
                    setConfirmWAN(true)
                  } else {
                    startTest()
                  }
                }}
                size="sm"
              >
                <Gauge data-icon="inline-start" />
                {running ? "A test is running…" : "Run test"}
              </Button>
            </div>
          </div>
        )}
      </PanelSection>
      {latest && <TestResults test={latest} />}
      <DeleteConfirmationDialog
        confirmLabel="Run on WAN"
        description="The WAN plane carries guest internet traffic and this node's public address. The test fills that link for its duration, and an upstream provider may meter or police it."
        isPending={start.isPending}
        onConfirm={startTest}
        onOpenChange={setConfirmWAN}
        open={confirmWAN}
        pendingLabel="Starting…"
        title="Run a network test on WAN?"
      />
    </>
  )
}

function TestResults({ test }: { test: NetworkTest }) {
  return (
    <PanelSection title={`Results · ${PLANE_LABELS[test.plane]}`}>
      <div className="mb-3 flex items-center gap-2 text-xs text-muted-foreground">
        <StateBadge
          state={test.status === "complete" ? "completed" : test.status}
        />
        <span>
          {test.streams} stream{test.streams === 1 ? "" : "s"} ·{" "}
          {test.durationSeconds}s each way · started{" "}
          {new Date(test.startedAt).toLocaleTimeString()}
        </span>
      </div>
      {test.error && <ErrorBanner msg={test.error} />}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        {test.runs.map((run) => (
          <RunResult key={`${run.from}->${run.to}`} run={run} />
        ))}
      </div>
    </PanelSection>
  )
}

function RunResult({ run }: { run: NetworkTestRun }) {
  const label = `${run.from} → ${run.to}`

  if (run.status !== "complete") {
    return (
      <div className="flex flex-col items-center justify-center gap-1 rounded-md border p-3 text-xs">
        <span className="font-medium">{label}</span>
        {run.status === "failed" ? (
          <span className="text-danger">{run.error ?? "Failed"}</span>
        ) : (
          <StateBadge state={run.status} />
        )}
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-2 rounded-md border p-3">
      <SpeedDial
        bitsPerSecond={run.bits_per_second}
        label={label}
        linkMbps={run.link_mbps}
      />
      <dl className="grid grid-cols-2 gap-x-3 gap-y-0.5 text-2xs">
        <dt className="text-muted-foreground">Link</dt>
        <dd className="text-end tabular-nums">
          {formatLinkSpeed(run.link_mbps)}
          {run.mtu ? ` · MTU ${run.mtu}` : ""}
        </dd>
        {run.rtt_avg_ms !== undefined && (
          <>
            <dt className="text-muted-foreground">Round trip</dt>
            <dd className="text-end tabular-nums">
              {run.rtt_avg_ms.toFixed(3)} ms avg ·{" "}
              {(run.rtt_max_ms ?? 0).toFixed(3)} max
            </dd>
          </>
        )}
        <dt className="text-muted-foreground">Packet loss</dt>
        <dd className="text-end tabular-nums">
          {(run.packet_loss_percent ?? 0).toFixed(0)}%
        </dd>
        <dt className="text-muted-foreground">From</dt>
        <dd className="text-end font-mono">{run.source_address ?? "-"}</dd>
      </dl>
    </div>
  )
}
