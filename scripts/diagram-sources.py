#!/usr/bin/env python3
"""Lint the docs diagram sources and build their icons.

  diagram-sources.py lint <src-dir>
  diagram-sources.py icons <src-dir> <lucide-icons-dir>

lint fails when a source other than style.d2 sets an icon or a style, names
a class that style.d2 does not define, or declares a resource with no label.
It also fails when style.d2 and icons.tsv disagree.

icons writes src/icons/<class>.svg and src/icons/group/<class>.svg from the
Lucide icon and colour that icons.tsv gives each class.
"""
import os
import re
import sys

STYLE = "style.d2"
TABLE = "icons.tsv"
LICENCE = "<!-- @license lucide-static "
# A group icon is a 22px glyph 2px in from the top-left of a 64px canvas,
# because d2 sets the size of a container icon. The label aligns to it.
GROUP_CANVAS = 64
GROUP_GLYPH = 22
GROUP_INSET = 2


def read_table(src):
    rows = {}
    path = os.path.join(src, TABLE)
    with open(path, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            line = line.rstrip("\n")
            if not line or line.startswith("#"):
                continue
            cols = line.split("\t")
            if len(cols) != 4 or cols[3] not in ("resource", "group"):
                raise SystemExit(f"{path}:{n}: want class, icon, colour, resource|group")
            if not re.fullmatch(r"#[0-9A-Fa-f]{6}", cols[2]):
                raise SystemExit(f"{path}:{n}: colour {cols[2]!r} is not #RRGGBB")
            rows[cols[0]] = (cols[1], cols[2], cols[3])
    return rows


def tokens(text):
    """Yield (line, kind, text) for a d2 file: kind is open, close or stmt.

    Strings become "S" ("" stays empty) and comments go, so that a label is
    present exactly when a head or value has text left in it.
    """
    buf, line, start = [], 1, 1
    i, n = 0, len(text)

    def flush(kind):
        nonlocal buf
        stmt = "".join(buf).strip()
        buf = []
        if stmt or kind == "open":
            return (start, kind, stmt)
        return None

    while i < n:
        c = text[i]
        if not buf or not "".join(buf).strip():
            start = line
        if c == '"':
            j = i + 1
            while j < n and text[j] != '"':
                j += 2 if text[j] == "\\" else 1
            buf.append('"S"' if j > i + 1 else '""')
            i = j + 1
            continue
        if c == "#":
            while i < n and text[i] != "\n":
                i += 1
            continue
        if c in "{};\n":
            tok = flush("open" if c == "{" else "stmt")
            if tok:
                yield tok
            if c == "}":
                yield (line, "close", "")
            if c == "\n":
                line += 1
            i += 1
            continue
        buf.append(c)
        i += 1


def key_value(stmt):
    m = re.match(r"^([^:]*?)\s*(?::\s*(.*))?$", stmt)
    return m.group(1).strip(), (m.group(2) or "").strip()


def class_names(value):
    value = value.strip()
    if value.startswith("[") and value.endswith("]"):
        return [v.strip() for v in re.split(r"[;,]", value[1:-1]) if v.strip()]
    return [value]


def style_classes(src):
    """Return {class: icon path or None} for the classes style.d2 defines."""
    with open(os.path.join(src, STYLE), encoding="utf-8") as f:
        toks = list(tokens(f.read()))
    classes, stack, current = {}, [], None
    for _, kind, stmt in toks:
        if kind == "open":
            key, _ = key_value(stmt)
            stack.append(key)
            if len(stack) == 2 and stack[0] == "classes":
                current = key
                classes[current] = None
        elif kind == "close":
            if len(stack) == 2 and stack[0] == "classes":
                current = None
            stack.pop()
        elif current and len(stack) == 2:
            key, value = key_value(stmt)
            if key == "icon":
                classes[current] = value
    return classes


def lint_style(src, classes, table):
    errs = []
    path = os.path.join(src, STYLE)
    for name, (_, _, kind) in table.items():
        want = f"icons/{name}.svg" if kind == "resource" else f"icons/group/{name}.svg"
        if name not in classes:
            errs.append(f"{path}: class {name} is in {TABLE} but not defined")
        elif classes[name] != want:
            errs.append(f"{path}: class {name} needs icon: {want}")
    for name, icon in classes.items():
        if icon and name not in table:
            errs.append(f"{path}: class {name} has an icon but no row in {TABLE}")
    return errs


def lint_source(path, classes, resources):
    errs = []
    with open(path, encoding="utf-8") as f:
        text = f.read()
    stack = []  # frames: [line, is_edge, labelled, resource]

    def err(line, msg):
        errs.append(f"{os.path.relpath(path)}:{line}: {msg}")

    for line, kind, stmt in tokens(text):
        if kind == "open":
            key, label = key_value(stmt)
            last = key.split(".")[-1]
            if last == "style" or ".style." in f".{key}.":
                err(line, "style is set in style.d2 only; pick a class")
            if key == "classes":
                err(line, "classes are defined in style.d2 only")
            edge = bool(re.search(r"->|<-|--", key))
            stack.append([line, edge, bool(label) and label != '""', False])
            continue
        if kind == "close":
            if not stack:
                err(line, "unmatched }")
                continue
            frame = stack.pop()
            if frame[3] and not frame[2]:
                err(frame[0], "a resource needs a label that starts with its service name")
            continue
        if stmt.startswith("...@"):
            continue
        key, value = key_value(stmt)
        parts = key.split(".")
        if "icon" in parts:
            err(line, "icon is set in style.d2 only; pick a class")
        if "style" in parts:
            err(line, "style is set in style.d2 only; pick a class")
        if parts[-1] == "label" and stack:
            stack[-1][2] = bool(value) and value != '""'
        if parts[-1] != "class":
            continue
        for name in class_names(value):
            if name not in classes:
                err(line, f"class {name} is not defined in style.d2")
            elif name in resources:
                if len(parts) > 1 or not stack:
                    err(line, f"declare the {name} node as name: \"label\" {{class: {name}}}")
                elif not stack[-1][1]:
                    stack[-1][3] = True
    return errs


def lint(src):
    table = read_table(src)
    classes = style_classes(src)
    errs = lint_style(src, classes, table)
    resources = {n for n, (_, _, kind) in table.items() if kind == "resource"}
    for name in sorted(os.listdir(src)):
        if name.endswith(".d2") and name != STYLE:
            errs += lint_source(os.path.join(src, name), classes, resources)
    for e in errs:
        print(f"diagram lint: {e}", file=sys.stderr)
    return 1 if errs else 0


def icons(src, lucide):
    out = os.path.join(src, "icons")
    os.makedirs(os.path.join(out, "group"), exist_ok=True)
    for old in os.listdir(out):
        if old.endswith(".svg"):
            os.remove(os.path.join(out, old))
    for old in os.listdir(os.path.join(out, "group")):
        os.remove(os.path.join(out, "group", old))
    for name, (icon, colour, kind) in read_table(src).items():
        path = os.path.join(lucide, f"{icon}.svg")
        with open(path, encoding="utf-8") as f:
            svg = f.read()
        if not svg.startswith(LICENCE):
            raise SystemExit(f"diagram icons: {path} has no Lucide licence comment")
        if svg.count('stroke="currentColor"') != 1:
            raise SystemExit(f"diagram icons: {path} does not stroke currentColor once")
        svg = svg.replace('stroke="currentColor"', f'stroke="{colour}"')
        if kind == "group":
            scale = GROUP_GLYPH / 24
            size = GROUP_CANVAS / scale
            off = -GROUP_INSET / scale
            svg = svg.replace('width="24"', f'width="{GROUP_CANVAS}"', 1)
            svg = svg.replace('height="24"', f'height="{GROUP_CANVAS}"', 1)
            svg = svg.replace('viewBox="0 0 24 24"', f'viewBox="{off:.4f} {off:.4f} {size:.4f} {size:.4f}"', 1)
            dest = os.path.join(out, "group", f"{name}.svg")
        else:
            dest = os.path.join(out, f"{name}.svg")
        with open(dest, "w", encoding="utf-8") as f:
            f.write(svg)
    return 0


def main():
    if len(sys.argv) == 3 and sys.argv[1] == "lint":
        sys.exit(lint(sys.argv[2]))
    if len(sys.argv) == 4 and sys.argv[1] == "icons":
        sys.exit(icons(sys.argv[2], sys.argv[3]))
    raise SystemExit(__doc__)


if __name__ == "__main__":
    main()
