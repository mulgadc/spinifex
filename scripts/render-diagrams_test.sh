#!/bin/sh
# render-diagrams_test.sh: tests for render-diagrams.sh --lint.
#
# Each case copies the real style.d2 and icons.tsv into a fixture directory,
# adds one source, and checks the lint result and the file:line it reports.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
RENDER="$SCRIPT_DIR/render-diagrams.sh"
REAL="$SCRIPT_DIR/../.github/assets/diagrams/src"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- render-diagrams.sh --lint"

# fixture <case> <source text>: a source directory holding one diagram.
fixture() {
	mkdir -p "$TMP/$1"
	cp "$REAL/style.d2" "$REAL/icons.tsv" "$TMP/$1/"
	printf '%s\n' "$2" >"$TMP/$1/d.d2"
}

# expect_fail <case> <line> <message part> <description>
expect_fail() {
	if DIAGRAMS_SRC="$TMP/$1" "$RENDER" --lint 2>"$TMP/$1.err"; then
		fail "$4: lint passed"
	elif grep -q "d.d2:$2: .*$3" "$TMP/$1.err"; then
		pass "$4"
	else
		fail "$4: want d.d2:$2 \"$3\", got: $(cat "$TMP/$1.err")"
	fi
}

fixture valid '...@style
direction: right
user: "Operator" {class: person}
vpc: "VPC 10.0.0.0/16" {
  class: vpc
  web: "EC2 instance\nweb-1" {class: instance}
  lb: {
    label: "ALB\nweb"
    class: lb
  }
  br: br-int {class: box; width: 120}
}
seq: "" {
  shape: sequence_diagram
  a: "A" {class: box}
}
user -> vpc.lb: "HTTPS" {class: flow}'
if DIAGRAMS_SRC="$TMP/valid" "$RENDER" --lint 2>"$TMP/valid.err"; then
	pass "a valid source passes"
else
	fail "a valid source passes: $(cat "$TMP/valid.err")"
fi

fixture icon '...@style
web: "EC2 instance" {
  class: instance
  icon: icons/server.svg
}'
expect_fail icon 4 "icon is set in style.d2 only" "an icon: key fails"

fixture style '...@style
a: "A" {class: box; style.fill: "#ff0000"}'
expect_fail style 2 "style is set in style.d2 only" "a style key fails"

fixture styleblock '...@style
a: "A" {
  class: box
  style: {stroke: "#ff0000"}
}'
expect_fail styleblock 4 "style is set in style.d2 only" "a style block fails"

fixture unknown '...@style
web: "EC2 instance" {class: res}'
expect_fail unknown 2 "class res is not defined" "an unknown class fails"

fixture edgeclass '...@style
a: "A" {class: box}
b: "B" {class: box}
a -> b: {class: arrow}'
expect_fail edgeclass 4 "class arrow is not defined" "an unknown edge class fails"

fixture nolabel '...@style
vpc: "VPC" {
  class: vpc
  web: {class: instance}
}'
expect_fail nolabel 4 "needs a label" "a resource with no label fails"

fixture emptylabel '...@style
web: "" {class: instance}'
expect_fail emptylabel 2 "needs a label" "a resource with an empty label fails"

fixture dotted '...@style
web: "EC2 instance"
web.class: instance'
expect_fail dotted 3 "declare the instance node" "a resource class set by path fails"

fixture classes '...@style
classes: {
  mine: {shape: image}
}'
expect_fail classes 2 "classes are defined in style.d2 only" "a classes block fails"

# style.d2 and icons.tsv must agree.
fixture table '...@style'
printf 'extra\tserver\t#ED7100\tresource\n' >>"$TMP/table/icons.tsv"
if DIAGRAMS_SRC="$TMP/table" "$RENDER" --lint 2>"$TMP/table.err"; then
	fail "an icons.tsv row with no class fails: lint passed"
elif grep -q "class extra is in icons.tsv but not defined" "$TMP/table.err"; then
	pass "an icons.tsv row with no class fails"
else
	fail "an icons.tsv row with no class fails: $(cat "$TMP/table.err")"
fi

if DIAGRAMS_SRC="$REAL" "$RENDER" --lint 2>"$TMP/real.err"; then
	pass "the committed sources pass"
else
	fail "the committed sources pass: $(cat "$TMP/real.err")"
fi

[ "$fails" -eq 0 ] || exit 1
