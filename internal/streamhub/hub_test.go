package streamhub

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestHub_PublishDeliversToEverySubscriber(t *testing.T) {
	h := NewHub()
	ch1, cancel1 := h.Subscribe()
	defer cancel1()
	ch2, cancel2 := h.Subscribe()
	defer cancel2()

	entry := domain.Entry{ID: "e1"}
	h.Publish(entry)

	for i, ch := range []<-chan domain.Entry{ch1, ch2} {
		select {
		case got := <-ch:
			if got.ID != entry.ID {
				t.Errorf("subscriber %d: got entry %q, want %q", i, got.ID, entry.ID)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: did not receive the published entry", i)
		}
	}
}

func TestHub_PublishWithNoSubscribersDoesNotBlockOrPanic(t *testing.T) {
	h := NewHub()
	h.Publish(domain.Entry{ID: "e1"})
}

func TestHub_CancelledSubscriberReceivesNothingAndChannelCloses(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the channel to be closed, got a value instead")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was not closed after cancel")
	}

	// A Publish after cancel must not resurrect or write to the closed
	// channel (that would panic) and must not affect any other
	// subscriber.
	other, cancelOther := h.Subscribe()
	defer cancelOther()
	h.Publish(domain.Entry{ID: "e1"})
	select {
	case got := <-other:
		if got.ID != "e1" {
			t.Errorf("got entry %q, want %q", got.ID, "e1")
		}
	case <-time.After(time.Second):
		t.Fatal("remaining subscriber did not receive the published entry")
	}
}

func TestHub_CancelIsIdempotent(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe()
	cancel()
	cancel() // must not double-close/panic
}

func TestHub_PublishToFullBufferDropsOldestRatherThanBlocking(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	defer cancel()

	// Fill the buffer beyond capacity: entries 0..subscriberBufferSize
	// (one more than fits) so the oldest (id "0") must be dropped to
	// make room for the newest.
	for i := 0; i <= subscriberBufferSize; i++ {
		h.Publish(domain.Entry{ID: strconv.Itoa(i)})
	}

	first := <-ch
	if first.ID == "0" {
		t.Errorf("expected the oldest entry to have been dropped, but it was still delivered first")
	}

	// Drain the rest; every remaining value must be a valid entry (no
	// panic, no deadlock) and the buffer must never have exceeded its
	// bound.
	count := 1
	for {
		select {
		case <-ch:
			count++
		default:
			if count > subscriberBufferSize {
				t.Errorf("buffered more entries (%d) than subscriberBufferSize (%d)", count, subscriberBufferSize)
			}
			return
		}
	}
}

func TestHub_ConcurrentPublishAndSubscribeIsRace_Free(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.Publish(domain.Entry{ID: "e"})
		}(i)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := h.Subscribe()
			defer cancel()
			select {
			case <-ch:
			case <-time.After(200 * time.Millisecond):
			}
		}()
	}
	wg.Wait()
}
