import { Link } from "@tanstack/react-router"
import { Folder } from "lucide-react"

interface FolderListItemProps {
  folderName: string
  fullPrefix: string
  bucketName: string
}

export function FolderListItem({
  folderName,
  fullPrefix,
  bucketName,
}: FolderListItemProps) {
  return (
    <Link
      className="flex items-center gap-3 rounded-lg border bg-card p-4 transition-colors hover:bg-accent"
      params={{ bucket: bucketName, _splat: fullPrefix }}
      to="/s3/ls/$bucket/$"
    >
      <Folder className="size-5 text-muted-foreground" />
      <span className="font-medium">{folderName}</span>
    </Link>
  )
}
