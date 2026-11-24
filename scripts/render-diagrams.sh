#!/usr/bin/env bash
# Renders .github/assets/diagrams/src/<name>.d2 to .github/assets/diagrams/<name>.svg.
#
# Usage: scripts/render-diagrams.sh [--lint | --check] [name ...]
#   --lint   only check the sources against style.d2 and icons.tsv.
#   --check  render to a temp dir and fail if a committed SVG differs.
#   name     render only these sources (default: all).
#
# Every render and check runs the lint first. Output is byte-stable for one
# d2 release only, so the release is pinned. Set D2_BIN to use a d2 binary
# that is not on PATH, and DIAGRAMS_SRC to lint another source directory.
set -euo pipefail

D2_VERSION="v0.7.1"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${DIAGRAMS_SRC:-$ROOT/.github/assets/diagrams/src}"
OUT="$ROOT/.github/assets/diagrams"
POST="$ROOT/scripts/diagram-postprocess.py"
TOOL="$ROOT/scripts/diagram-sources.py"

D2_FLAGS=(
	--omit-version --pad 10 --theme 0 --layout elk
	--elk-nodeNodeBetweenLayers 30
	--elk-edgeNodeBetweenLayers 20
	--elk-padding "[top=45,left=20,bottom=20,right=20]"
)

check=0
case "${1:-}" in
--lint)
	exec python3 -I "$TOOL" lint "$SRC"
	;;
--check)
	check=1
	shift
	;;
esac

python3 -I "$TOOL" lint "$SRC"

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
		[[ "$n" == "style" ]] && continue # shared classes, not a diagram
		names+=("$n")
	done
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Icons are not committed: each SVG embeds the icons it uses. Fetch the pinned
# lucide-static package once, then write the gitignored src/icons/ from the
# icon and colour that icons.tsv gives each class.
LUCIDE_VERSION="1.78.0"
LUCIDE_URL="https://registry.npmjs.org/lucide-static/-/lucide-static-$LUCIDE_VERSION.tgz"
LUCIDE_SHA256="8312fadb97e45c76e2f847f883ece725be167cb360aac53d2540e54e357bc778"
LUCIDE_CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/spinifex-diagrams/lucide-static-$LUCIDE_SHA256"

if [[ ! -d "$LUCIDE_CACHE/icons" ]]; then
	mkdir -p "$LUCIDE_CACHE"
	curl -sSfL -o "$tmp/lucide.tgz" "$LUCIDE_URL"
	echo "$LUCIDE_SHA256  $tmp/lucide.tgz" | sha256sum -c --quiet - || {
		echo "render-diagrams: lucide-static checksum mismatch" >&2
		exit 1
	}
	rm -rf "$LUCIDE_CACHE/x.partial"
	mkdir -p "$LUCIDE_CACHE/x.partial"
	tar -xzf "$tmp/lucide.tgz" -C "$LUCIDE_CACHE/x.partial" package/icons
	mv "$LUCIDE_CACHE/x.partial/package/icons" "$LUCIDE_CACHE/icons"
	rm -rf "$LUCIDE_CACHE/x.partial"
fi

python3 -I "$TOOL" icons "$SRC" "$LUCIDE_CACHE/icons"

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
