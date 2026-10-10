package instancestate

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

type report struct {
	unrecorded []use
	stale      []allowance
	malformed  []string
}

// check compares the scanned uses with allow.
func check(s scan, allow []allowance) report {
	var rep report
	allowed := map[use]bool{}
	for _, a := range allow {
		u := use{File: a.File, Symbol: a.Symbol}
		switch {
		case allowed[u]:
			rep.malformed = append(rep.malformed, "duplicate entry "+u.String())
		case a.Inv == "":
			rep.malformed = append(rep.malformed, "entry "+u.String()+" cites no inventory item")
		}
		allowed[u] = true
	}
	for _, u := range s.sorted() {
		if !allowed[u] {
			rep.unrecorded = append(rep.unrecorded, u)
		}
	}
	for _, a := range allow {
		if _, ok := s[use{File: a.File, Symbol: a.Symbol}]; !ok {
			rep.stale = append(rep.stale, a)
		}
	}
	return rep
}

// TestADR0007_S6_NoNewRawInstanceStateConsumers is the blocking ratchet: no
// production file may start naming the daemon's instance-state bucket, keys,
// local file, raw record or JetStreamManager instance methods.
func TestADR0007_S6_NoNewRawInstanceStateConsumers(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	s, files, err := scanModule(root)
	if err != nil {
		t.Fatal(err)
	}
	// Guards against a walk that silently finds nothing and passes vacuously.
	if files < 500 || len(s) < 20 {
		t.Fatalf("scanned %d files and found %d uses under %s; the walk has lost the tree", files, len(s), root)
	}
	if len(allowlist) == 0 {
		t.Fatal("the allowlist is empty, so the stale check would pass vacuously")
	}
	rep := check(s, allowlist)

	t.Run("NoUnrecordedConsumer", func(t *testing.T) {
		for _, u := range rep.unrecorded {
			t.Errorf("ADR-0007 S6 (consumers receive narrow state capabilities rather than raw stores): %s at %s; "+
				"consume an instance-domain capability instead of the daemon's raw instance state", u, s[u])
		}
	})
	t.Run("Allowlist_NoStaleEntries", func(t *testing.T) {
		for _, a := range rep.stale {
			t.Errorf("allowlist entry %s uses %s (%s) no longer matches; delete it and update the inventory",
				a.File, a.Symbol, a.Inv)
		}
	})
	t.Run("Allowlist_WellFormed", func(t *testing.T) {
		for _, m := range rep.malformed {
			t.Errorf("allowlist_test.go: %s", m)
		}
		t.Logf("%d allow-listed uses", len(allowlist))
	})
	t.Run("OwnerFilesExist", func(t *testing.T) {
		for _, f := range ownerFiles {
			if _, err := os.Stat(filepath.Join(root, f)); err != nil {
				t.Errorf("owner file %s is gone; remove it from ownerFiles and re-scan its consumers: %v", f, err)
			}
		}
	})
}

const faultSource = `package %s

import (
	"encoding/json"

	spxd "github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

type holder struct {
	LoadInstanceRecord func() // a field name, not a use
	m                  *spxd.JetStreamManager
}

func (h holder) WriteRunningSet() {} // a declaration, not a use

func use(h holder, raw []byte, jsManager *spxd.JetStreamManager, other interface{ LoadState() }) {
	_ = spxd.InstanceStateBucket
	_ = spxd.LocalStatePath("")
	var r vm.InstanceRecord
	_ = json.Unmarshal(raw, &r)
	_ = vm.VMFromRecord(&r)
	_ = (&vm.VM{}).Record()
	_, _ = h.m.LoadInstanceRecord("i-1")
	_, _, _ = jsManager.LoadState("node")
	other.LoadState()
	_ = "spinifex-instance-state"
}
`

func parseFault(t *testing.T, rel, pkg string) scan {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, fmt.Sprintf(faultSource, pkg), parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	out := scan{}
	scanFile(fset, rel, f, out)
	return out
}

// TestADR0007_S6_RatchetDetectsFaults injects a new consumer, a stale entry
// and a malformed entry, and checks the matcher's declaration exclusions.
func TestADR0007_S6_RatchetDetectsFaults(t *testing.T) {
	const rel = "spinifex/domains/widget/w.go"
	s := parseFault(t, rel, "widget")

	want := []string{
		"daemon.InstanceStateBucket", "daemon.LocalStatePath", "daemon.JetStreamManager",
		"vm.InstanceRecord", "vm.VMFromRecord", "VM.Record()",
		"JetStreamManager.LoadInstanceRecord", "JetStreamManager.LoadState",
		`literal "spinifex-instance-state"`,
	}
	for _, sym := range want {
		if _, ok := s[use{File: rel, Symbol: sym}]; !ok {
			t.Errorf("injected use of %s was not detected; got %v", sym, s.sorted())
		}
	}
	if len(s) != len(want) {
		t.Errorf("want exactly %d uses (field names, method declarations and an unrelated LoadState excluded), got %v",
			len(want), s.sorted())
	}

	stale := allowance{File: "spinifex/domains/gone/g.go", Symbol: "vm.InstanceRecord", Inv: "INV-00"}
	recorded := allowance{File: rel, Symbol: "vm.InstanceRecord", Inv: "INV-00"}
	rep := check(s, []allowance{recorded, stale, {File: rel, Symbol: "vm.VMFromRecord"}, recorded})
	if len(rep.unrecorded) != len(want)-2 {
		t.Errorf("want %d unrecorded uses, got %v", len(want)-2, rep.unrecorded)
	}
	if len(rep.stale) != 1 || rep.stale[0] != stale {
		t.Errorf("want stale entry %+v, got %+v", stale, rep.stale)
	}
	if len(rep.malformed) != 2 {
		t.Errorf("want a missing-inventory and a duplicate entry reported, got %v", rep.malformed)
	}
}

// Inside the owning packages the symbols are bare identifiers, and the scan
// must see those too.
func TestADR0007_S6_RatchetSeesBareIdentifiersInOwnerPackages(t *testing.T) {
	src := map[string]string{
		"spinifex/daemon/new_consumer.go":           "package daemon\n\nfunc f() string { return InstanceStateBucket + LocalStatePath(\"\") }\n",
		"spinifex/runtime/compute/vm/new_reader.go": "package vm\n\nfunc g(r *InstanceRecord) *VM { return VMFromRecord(r) }\n",
	}
	want := map[string][]string{
		"spinifex/daemon/new_consumer.go":           {"daemon.InstanceStateBucket", "daemon.LocalStatePath"},
		"spinifex/runtime/compute/vm/new_reader.go": {"vm.InstanceRecord", "vm.VMFromRecord"},
	}
	for rel, code := range src {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, code, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		out := scan{}
		scanFile(fset, rel, f, out)
		for _, sym := range want[rel] {
			if _, ok := out[use{File: rel, Symbol: sym}]; !ok {
				t.Errorf("%s: bare use of %s not detected; got %v", rel, sym, out.sorted())
			}
		}
	}
}
