package agent

import (
	"testing"
	"time"
)

func TestRegistryListStableOrder(t *testing.T) {
	reg := NewRegistry()

	early := New(Options{ID: "server-b", Kind: KindNative, Backend: &fakeBackend{}})
	early.ConnectedAt = time.Unix(100, 0)
	late := New(Options{ID: "server-a", Kind: KindNative, Backend: &fakeBackend{}})
	late.ConnectedAt = time.Unix(200, 0)
	tie1 := New(Options{ID: "bot-z", Kind: KindShell, Backend: &fakeBackend{}})
	tie1.ConnectedAt = time.Unix(300, 0)
	tie2 := New(Options{ID: "bot-a", Kind: KindShell, Backend: &fakeBackend{}})
	tie2.ConnectedAt = time.Unix(300, 0)

	// Register in a scrambled order.
	reg.Register(tie2)
	reg.Register(late)
	reg.Register(tie1)
	reg.Register(early)

	// Call repeatedly; map iteration is random, but the result must be stable.
	want := []string{"server-b", "server-a", "bot-a", "bot-z"}
	for i := 0; i < 20; i++ {
		got := reg.List()
		if len(got) != len(want) {
			t.Fatalf("unexpected length %d", len(got))
		}
		for j, a := range got {
			if a.ID != want[j] {
				t.Fatalf("iteration %d: order %v, want %v", i, ids(got), want)
			}
		}
	}
}

// A stale connection's cleanup must not evict its successor after a reconnect.
func TestUnregisterAgentIdentity(t *testing.T) {
	reg := NewRegistry()
	a1 := New(Options{ID: "x", Kind: KindNative, Backend: &fakeBackend{}})
	a2 := New(Options{ID: "x", Kind: KindNative, Backend: &fakeBackend{}})

	reg.Register(a1)
	reg.Register(a2) // replaces a1 (and closes it)
	if reg.Get("x") != a2 {
		t.Fatal("a2 should be the registered agent")
	}

	reg.UnregisterAgent(a1) // stale cleanup from the old connection
	if reg.Get("x") != a2 {
		t.Fatal("stale unregister evicted the successor")
	}

	reg.UnregisterAgent(a2) // the real successor unregisters itself
	if reg.Get("x") != nil {
		t.Fatal("a2 should be removed by its own unregister")
	}
}

func ids(list []*Agent) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.ID
	}
	return out
}
