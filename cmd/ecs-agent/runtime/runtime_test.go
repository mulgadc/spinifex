package runtime

import (
	"context"
	"errors"
	"testing"
)

// staticResolver is a test Resolver returning fixed creds or an error.
type staticResolver struct {
	user, pass, endpoint string
	err                  error
}

func (s staticResolver) Authorize(_ context.Context, _ string) (string, string, string, error) {
	return s.user, s.pass, s.endpoint, s.err
}

func TestFakePuller_RecordsAndDefaultsRef(t *testing.T) {
	f := &FakePuller{}
	img, err := f.Pull(context.Background(), PullSpec{Ref: "repo/app:1"}, staticResolver{user: "AWS"})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if img.Ref != "repo/app:1" {
		t.Errorf("Ref defaulted wrong: %q", img.Ref)
	}
	if len(f.Pulls) != 1 || f.Pulls[0].Ref != "repo/app:1" {
		t.Errorf("Pulls = %+v", f.Pulls)
	}
	if len(f.Authzd) != 1 || f.Authzd[0] != "AWS" {
		t.Errorf("Authzd = %+v", f.Authzd)
	}
}

func TestFakePuller_ResolverError(t *testing.T) {
	f := &FakePuller{}
	_, err := f.Pull(context.Background(), PullSpec{Ref: "r"}, staticResolver{err: errors.New("boom")})
	if err == nil {
		t.Fatal("expected resolver error to propagate")
	}
}

func TestFakePuller_ProgrammedError(t *testing.T) {
	f := &FakePuller{Err: errors.New("pull failed")}
	if _, err := f.Pull(context.Background(), PullSpec{Ref: "r"}, nil); err == nil {
		t.Fatal("expected programmed error")
	}
}

func TestFakePuller_OnPullOverrides(t *testing.T) {
	want := Image{Ref: "x", Digest: "sha256:abc", Size: 42}
	f := &FakePuller{Err: errors.New("ignored"), OnPull: func(PullSpec) (Image, error) {
		return want, nil
	}}
	got, err := f.Pull(context.Background(), PullSpec{Ref: "r"}, nil)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestFakePuller_Close(t *testing.T) {
	f := &FakePuller{}
	if err := f.Close(); err != nil || !f.Closed {
		t.Fatalf("Close: err=%v closed=%v", err, f.Closed)
	}
}

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"nginx:alpine":      "docker.io/library/nginx:alpine",
		"nginx":             "docker.io/library/nginx:latest",
		"ghcr.io/org/app:1": "ghcr.io/org/app:1",
		"123456789012.dkr.ecr.ap-southeast-2.spinifex.internal/app:1": "123456789012.dkr.ecr.ap-southeast-2.spinifex.internal/app:1",
	}
	for in, want := range cases {
		got, err := normalizeRef(in)
		if err != nil {
			t.Errorf("normalizeRef(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := normalizeRef("Bad::Ref"); err == nil {
		t.Error("normalizeRef accepted invalid ref")
	}
}
