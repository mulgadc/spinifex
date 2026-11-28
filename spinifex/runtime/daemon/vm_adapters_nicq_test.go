package daemon

import (
	"reflect"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// ipsec_enabled must reach the VM manager only as the guest MTU. A setting
// such as a queue gate is a plain value, so every scalar field other than
// GuestMTU must be the same with IPsec on and off.
func TestBuildVMManagerDeps_IPsecChangesOnlyGuestMTU(t *testing.T) {
	depsFor := func(ipsec bool) vm.Deps {
		d := &Daemon{
			config: &config.Config{},
			vmMgr:  vm.NewManager(),
			clusterConfig: &config.ClusterConfig{
				Network: config.NetworkConfig{IPSecEnabled: ipsec, UnderlayMTU: config.DefaultUnderlayMTU},
			},
		}
		return d.buildVMManagerDeps()
	}
	on, off := reflect.ValueOf(depsFor(true)), reflect.ValueOf(depsFor(false))

	assert := func(cond bool, format string, args ...any) {
		t.Helper()
		if !cond {
			t.Errorf(format, args...)
		}
	}
	assert(on.FieldByName("GuestMTU").Int() == 1408, "GuestMTU with IPsec on = %d, want 1408", on.FieldByName("GuestMTU").Int())
	assert(off.FieldByName("GuestMTU").Int() == 1442, "GuestMTU with IPsec off = %d, want 1442", off.FieldByName("GuestMTU").Int())
	for i := range on.NumField() {
		f := on.Type().Field(i)
		switch f.Type.Kind() {
		case reflect.Bool, reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint64, reflect.Float64, reflect.String:
		default:
			continue
		}
		if f.Name == "GuestMTU" {
			continue
		}
		assert(reflect.DeepEqual(on.Field(i).Interface(), off.Field(i).Interface()),
			"vm.Deps.%s differs with IPsec on (%v) and off (%v)", f.Name, on.Field(i).Interface(), off.Field(i).Interface())
	}
}
