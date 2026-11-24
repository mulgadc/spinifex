#!/usr/bin/env python3
"""Put each group label beside its group icon in a d2-rendered SVG.

d2 centres a top-left container label on its own width estimate, so the
label sits under the icon. This moves it to start just right of the icon.
"""
import re
import sys

# Group classes in src/style.d2 that carry a top-left icon.
ICON_GROUPS = (
    "region",
    "vpc",
    "public-subnet",
    "private-subnet",
    "server",
    "datacenter",
    "instance-contents",
    "auto-scaling",
)
# Icon inset plus the 26px icon plus a gap.
ICON_GAP = 34

_GROUP = re.compile(
    r'(<g class="\S+ (?:' + "|".join(ICON_GROUPS) + r')"><g class="shape" ><rect x="([0-9.]+)".*?)'
    r'<text x="[0-9.]+"([^>]*?)text-anchor:middle',
    re.S,
)
_ANY_ELEMENT = re.compile(r'<g class="\S+ [\w-]+">')
_TSPAN_X = re.compile(r'<tspan x="[0-9.]+"')


def align(svg: str) -> str:
    out, pos = [], 0
    for m in _GROUP.finditer(svg):
        head = m.group(1)
        # The first text after this group belongs to a later element: no label.
        if _ANY_ELEMENT.search(head, 3):
            continue
        x = float(m.group(2)) + ICON_GAP
        out.append(svg[pos : m.start()])
        out.append(f'{head}<text x="{x:.6f}"{m.group(3)}text-anchor:start')
        # A multi-line label puts each line in a tspan with its own x.
        end = svg.index("</text>", m.end())
        out.append(_TSPAN_X.sub(f'<tspan x="{x:.6f}"', svg[m.end() : end]))
        pos = end
    out.append(svg[pos:])
    return "".join(out)


def main() -> None:
    for path in sys.argv[1:]:
        with open(path, encoding="utf-8") as f:
            svg = f.read()
        with open(path, "w", encoding="utf-8") as f:
            f.write(align(svg))


if __name__ == "__main__":
    main()
