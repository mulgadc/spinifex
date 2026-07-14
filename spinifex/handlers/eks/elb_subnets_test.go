package handlers_eks

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type azResolver struct {
	az  map[string]string
	err map[string]error
}

func (r azResolver) GetSubnetVPC(_ context.Context, _, _ string) (string, error) {
	return "vpc-aaa", nil
}
func (r azResolver) GetVPCCIDR(_ context.Context, _, _ string) (string, error) {
	return "10.0.0.0/16", nil
}
func (r azResolver) GetSubnetAZ(_ context.Context, _, subnetID string) (string, error) {
	if r.err != nil {
		if e, ok := r.err[subnetID]; ok {
			return "", e
		}
	}
	return r.az[subnetID], nil
}

func TestDedupSubnetsByAZ(t *testing.T) {
	t.Run("single-AZ collapses to one subnet", func(t *testing.T) {
		r := azResolver{az: map[string]string{"subnet-a": "z1", "subnet-b": "z1"}}
		got := dedupSubnetsByAZ(context.Background(), r, "acct", []string{"subnet-a", "subnet-b"})
		if want := []string{"subnet-a"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("multi-AZ keeps one per AZ in order", func(t *testing.T) {
		r := azResolver{az: map[string]string{
			"subnet-a": "z1", "subnet-b": "z2", "subnet-c": "z1", "subnet-d": "z2",
		}}
		got := dedupSubnetsByAZ(context.Background(), r, "acct", []string{"subnet-a", "subnet-b", "subnet-c", "subnet-d"})
		if want := []string{"subnet-a", "subnet-b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("unresolved AZ is skipped", func(t *testing.T) {
		r := azResolver{
			az:  map[string]string{"subnet-b": "z2"},
			err: map[string]error{"subnet-a": errors.New("boom")},
		}
		got := dedupSubnetsByAZ(context.Background(), r, "acct", []string{"subnet-a", "subnet-b"})
		if want := []string{"subnet-b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("nil resolver yields nil", func(t *testing.T) {
		if got := dedupSubnetsByAZ(context.Background(), nil, "acct", []string{"subnet-a"}); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
}
