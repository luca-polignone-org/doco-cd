package rbac

import (
	"context"
	"reflect"
	"testing"
)

func TestPrincipalContext(t *testing.T) {
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("empty context must not carry a principal")
	}

	want := Principal{Subject: "alice", Groups: []string{"g1"}}

	got, ok := PrincipalFrom(WithPrincipal(context.Background(), want))
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, %v; want %+v, true", got, ok, want)
	}
}
