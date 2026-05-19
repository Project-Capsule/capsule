package container

import (
	"reflect"
	"testing"

	gocni "github.com/containerd/go-cni"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// The orphaned-DNAT bug was a setup/teardown asymmetry: CNI ADD got the
// port-map capability, DEL didn't. Remove has no live spec, so it
// rebuilds the mappings from the PortMapLabel. These tests pin the
// invariant that what cniSetup sends and what Remove replays are
// byte-for-byte the same.

func TestPortMappingEncodeDecodeRoundTrip(t *testing.T) {
	ports := []*capsulev1.PortMapping{
		{HostPort: 2222, ContainerPort: 22, Protocol: "tcp"},
		{HostPort: 9000, ContainerPort: 9000, Protocol: "udp"},
		{HostPort: 80, ContainerPort: 8080}, // empty proto -> tcp default
	}

	// What cniSetup hands CNI on ADD.
	setup := gocniPortMappings(ports)
	// What Remove reconstructs from the persisted label on DEL.
	teardown := decodePortMappings(encodePortMappings(ports))

	if !reflect.DeepEqual(setup, teardown) {
		t.Fatalf("ADD/DEL port mappings diverged:\n add = %#v\n del = %#v", setup, teardown)
	}

	want := []gocni.PortMapping{
		{HostPort: 2222, ContainerPort: 22, Protocol: "tcp"},
		{HostPort: 9000, ContainerPort: 9000, Protocol: "udp"},
		{HostPort: 80, ContainerPort: 8080, Protocol: "tcp"},
	}
	if !reflect.DeepEqual(setup, want) {
		t.Fatalf("unexpected mappings: got %#v want %#v", setup, want)
	}
}

func TestPortMappingEmptyIsSymmetric(t *testing.T) {
	// No ports: encode must be "" and both sides must yield nil opts so
	// ADD and DEL stay identical (no spurious portmap capability).
	if s := encodePortMappings(nil); s != "" {
		t.Fatalf("encode(nil) = %q, want \"\"", s)
	}
	if m := decodePortMappings(""); m != nil {
		t.Fatalf("decode(\"\") = %#v, want nil", m)
	}
	if o := portMapNSOpts(nil); o != nil {
		t.Fatalf("portMapNSOpts(nil) = %#v, want nil", o)
	}
}

func TestDecodePortMappingsSkipsGarbage(t *testing.T) {
	// Teardown is best-effort: a malformed label must not panic and must
	// not fabricate bogus mappings.
	got := decodePortMappings("not-a-mapping,2222:22/tcp,bad:port/tcp,,7:7/sctp")
	want := []gocni.PortMapping{
		{HostPort: 2222, ContainerPort: 22, Protocol: "tcp"},
		{HostPort: 7, ContainerPort: 7, Protocol: "sctp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}
