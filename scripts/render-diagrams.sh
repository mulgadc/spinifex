#!/usr/bin/env bash
# Renders .github/assets/diagrams/src/<name>.d2 to .github/assets/diagrams/<name>.svg.
#
# Usage: scripts/render-diagrams.sh [--check] [name ...]
#   --check  render to a temp dir and fail if a committed SVG differs.
#   name     render only these sources (default: all).
#
# Output is byte-stable for one d2 release only, so the release is pinned.
# Set D2_BIN to use a d2 binary that is not on PATH.
set -euo pipefail

D2_VERSION="v0.7.1"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/.github/assets/diagrams/src"
OUT="$ROOT/.github/assets/diagrams"
POST="$ROOT/scripts/diagram-postprocess.py"

D2_FLAGS=(
	--omit-version --pad 10 --theme 0 --layout elk
	--elk-nodeNodeBetweenLayers 30
	--elk-edgeNodeBetweenLayers 20
	--elk-padding "[top=45,left=20,bottom=20,right=20]"
)

check=0
if [[ "${1:-}" == "--check" ]]; then
	check=1
	shift
fi

d2="${D2_BIN:-$(command -v d2 || true)}"
if [[ -z "$d2" ]]; then
	echo "render-diagrams: d2 $D2_VERSION not found; set D2_BIN" >&2
	exit 1
fi
have="$("$d2" --version 2>/dev/null || true)"
if [[ "$have" != "$D2_VERSION" ]]; then
	echo "render-diagrams: need d2 $D2_VERSION, found '${have:-none}' at $d2" >&2
	exit 1
fi

if [[ $# -gt 0 ]]; then
	names=("$@")
else
	names=()
	for f in "$SRC"/*.d2; do
		n="$(basename "$f" .d2)"
		[[ "$n" == "aws" ]] && continue # shared classes, not a diagram
		names+=("$n")
	done
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Icons are not committed: each SVG embeds the icons it uses. Fetch the pinned
# AWS Architecture Icons package once, then fill the gitignored src/icons/ with
# every icon a source names. A group icon is drawn small on a 64px canvas.
ICONS_URL="https://d1.awsstatic.com/onedam/marketing-channels/website/public/shared/architecture-icon-release/Icon-package_07312026.5846e92413caa21490223536cc97f1269e44fa92.zip"
ICONS_SHA256="d2d166c453526471749d520e0db022c459abef759d2946cf2dd1d1c992dc6526"
ICONS_CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/spinifex-diagrams/$ICONS_SHA256"

if [[ ! -d "$ICONS_CACHE/x" ]]; then
	mkdir -p "$ICONS_CACHE"
	curl -sSfL -o "$tmp/icons.zip" "$ICONS_URL"
	echo "$ICONS_SHA256  $tmp/icons.zip" | sha256sum -c --quiet - || {
		echo "render-diagrams: icon package checksum mismatch" >&2
		exit 1
	}
	unzip -q "$tmp/icons.zip" -x '__MACOSX/*' -d "$ICONS_CACHE/x.partial"
	mv "$ICONS_CACHE/x.partial" "$ICONS_CACHE/x"
fi

mkdir -p "$SRC/icons/group"
while read -r ref; do
	dest="$SRC/$ref"
	[[ -f "$dest" ]] && continue
	name="$(basename "$ref")"
	found="$(find "$ICONS_CACHE/x" -name "$name" | sort | head -1)"
	[[ -n "$found" ]] || {
		echo "render-diagrams: no icon $name in the AWS package" >&2
		exit 1
	}
	if [[ "$ref" == icons/group/* ]]; then
		printf '<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64" viewBox="0 0 64 64"><image x="0" y="0" width="26" height="26" href="data:image/svg+xml;base64,%s"/></svg>\n' \
			"$(base64 -w0 "$found")" >"$dest"
	else
		cp "$found" "$dest"
	fi
done < <(grep -ohE 'icons/[A-Za-z0-9_./-]+\.svg' "$SRC"/*.d2 | sort -u)

stale=0
for n in "${names[@]}"; do
	src="$SRC/$n.d2"
	[[ -f "$src" ]] || {
		echo "render-diagrams: no source $src" >&2
		exit 1
	}
	# d2 resolves imports and icons from the source directory.
	(cd "$SRC" && "$d2" "${D2_FLAGS[@]}" "$n.d2" "$tmp/$n.svg" 2>"$tmp/$n.log") || {
		cat "$tmp/$n.log" >&2
		exit 1
	}
	python3 -I "$POST" "$tmp/$n.svg"
	if [[ $check -eq 1 ]]; then
		if ! cmp -s "$tmp/$n.svg" "$OUT/$n.svg"; then
			echo "stale: $OUT/$n.svg (run scripts/render-diagrams.sh $n)" >&2
			stale=1
		fi
	else
		cp "$tmp/$n.svg" "$OUT/$n.svg"
		echo "rendered $n.svg"
	fi
done

exit $stale
