package sse_test

import (
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/sse"
)

func TestPublishDeliversToUserSubscribersOnly(t *testing.T) {
	h := sse.NewHub(4)
	chA, cancelA := h.Subscribe("user-a")
	defer cancelA()
	chB, cancelB := h.Subscribe("user-b")
	defer cancelB()

	h.Publish("user-a", sse.Event{Kind: "phase", Data: "library"})

	select {
	case ev := <-chA:
		if ev.Kind != "phase" || ev.Data != "library" {
			t.Fatalf("chA got = %+v", ev)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("chA did not receive")
	}
	select {
	case ev := <-chB:
		t.Fatalf("chB should not have received: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishDropsInsteadOfBlockingOnFullBuffer(t *testing.T) {
	h := sse.NewHub(1)
	ch, cancel := h.Subscribe("u")
	defer cancel()

	// First publish fills the buffer (1 slot).
	h.Publish("u", sse.Event{Kind: "1"})
	// Second and third should not block — they drop.
	h.Publish("u", sse.Event{Kind: "2"})
	h.Publish("u", sse.Event{Kind: "3"})

	ev := <-ch
	if ev.Kind != "1" {
		t.Fatalf("first event = %+v, want kind=1", ev)
	}
	// No more events queued.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected second event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCancelClosesChannelAndCleansRegistry(t *testing.T) {
	h := sse.NewHub(4)
	ch, cancel := h.Subscribe("u")
	if h.SubscriberCount("u") != 1 {
		t.Fatalf("subs = %d, want 1", h.SubscriberCount("u"))
	}
	cancel()
	if h.SubscriberCount("u") != 0 {
		t.Fatalf("subs after cancel = %d, want 0", h.SubscriberCount("u"))
	}
	// Reading from a closed channel returns zero-value with ok=false.
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel after cancel")
	}
}

func TestMultipleSubscribersPerUser(t *testing.T) {
	h := sse.NewHub(4)
	ch1, c1 := h.Subscribe("u")
	defer c1()
	ch2, c2 := h.Subscribe("u")
	defer c2()

	h.Publish("u", sse.Event{Kind: "x", Data: "y"})
	for _, ch := range []<-chan sse.Event{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.Kind != "x" {
				t.Fatalf("kind = %q", ev.Kind)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("one channel did not receive")
		}
	}
}
