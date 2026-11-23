import { useQuery } from "@tanstack/react-query"

import { pickerNotice, PickerNoticeText } from "@/components/picker-notice"
import { SwitchList } from "@/components/switch-list"
import { routeTableDetail } from "@/lib/vpc-endpoints"
import { ec2RouteTablesQueryOptions } from "@/queries/ec2"

interface RouteTablePickerProps {
  vpcId: string
  selected: string[]
  onChange: (next: string[]) => void
}

// The VPC's route tables, each one a switch. A chosen table gets a route to
// the service's prefix list.
export function RouteTablePicker({
  vpcId,
  selected,
  onChange,
}: RouteTablePickerProps) {
  const query = useQuery(ec2RouteTablesQueryOptions)

  const tables = (query.data?.RouteTables ?? []).filter(
    (table) => table.VpcId === vpcId,
  )

  const notice = pickerNotice(query, false, {
    loading: "Reading route tables…",
    failed: "Could not read route tables",
    empty: "",
  })

  if (notice) {
    return <PickerNoticeText notice={notice} />
  }

  return (
    <SwitchList
      emptyText="This VPC has no route tables."
      itemLabel="Route table"
      items={tables.map((table) => ({
        id: table.RouteTableId ?? "",
        label: table.RouteTableId ?? "",
        detail: routeTableDetail(table) || undefined,
      }))}
      onChange={onChange}
      scroll
      selected={selected}
    />
  )
}
