package session

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// TestResendFramesSurvivesABlockRoll pins the one thing that makes the frame
// store's block recycling safe: ResendFrames copies the frames it hands out.
//
// The store reuses the buffer of a block it drops, so a Frame.Body is only valid
// until the next tick rolls a block. The reconnect handler sends the frames it
// was given while the main loop keeps ticking, so without the copy the bytes it
// sends would be the next block's. This test takes the resend, rolls the block
// that held it, and reads the frames back.
func TestResendFramesSurvivesABlockRoll(t *testing.T) {
	s := NewSession(1)
	s.maxPagesPerMatch = 1 // one block: every roll recycles the same buffer
	s.pageSize = 200

	// Two frames per block: 43 bytes plus the 57 of overhead is 100, so a
	// 200-byte block holds two.
	const frames = 2
	for i := range frames {
		s.RecordFrame(0x0A, storeFrame(43, byte(i+1)))
	}
	if got := s.ReplayPageCount(); got != frames {
		t.Fatalf("the store holds %d frames, want %d", got, frames)
	}

	resent, ok := s.ResendFrames(0)
	if !ok {
		t.Fatalf("ResendFrames(0) refused a range the store holds")
	}
	if len(resent) != frames {
		t.Fatalf("ResendFrames returned %d frames, want %d", len(resent), frames)
	}

	// Roll the block the resent frames came from. With one block of two frames,
	// the third frame drops it and recycles its buffer.
	for i := range 4 {
		s.RecordFrame(0x0A, storeFrame(43, byte(0xA0+i)))
	}

	// The store's own frames are the new ones, which is what proves the buffer
	// was recycled and not left alone: the same bytes the resend copied are now
	// something else.
	cur := s.replay.GetFrames(4, 5)
	if len(cur) != 2 {
		t.Fatalf("the store holds %d frames after the roll, want 2", len(cur))
	}
	if !bytes.Equal(cur[0].Body, storeFrame(43, 0xA2)) {
		t.Fatalf("the store's first frame is %x, want the one written after the roll: the buffer was not reused, "+
			"so this test is not exercising the recycling it exists for", cur[0].Body)
	}

	for i, f := range resent {
		want := storeFrame(43, byte(i+1))
		if !bytes.Equal(f.Body, want) {
			t.Fatalf("resent frame %d was clobbered by the block roll: got %x, want %x", i, f.Body, want)
		}
		if f.Type != 0x0A {
			t.Fatalf("resent frame %d has type 0x%02x, want 0x0A", i, f.Type)
		}
	}
}

// TestResendFramesRacesTheTick is the same property under -race, and it is the
// one the sequential test above cannot see.
//
// The block roll happens on the tick, and the tick reserves inside the open block
// with no session lock held: PopAckTick -> PopTickInto -> Replay.Reserve, and
// Reserve is what drops the oldest block and hands its buffer to the new one. A
// resend that took the frames from GetFrames and copied them afterwards would
// therefore be reading memory the tick may already be writing, which is a data
// race and, worse, a resend of bytes from a block that no longer holds them. The
// store is one small block here so every couple of ticks rolls it.
//
// Run with -race: without the copy inside the store's lock this test reports the
// write in dropOldestBlockLocked against the read in ResendFrames.
func TestResendFramesRacesTheTick(t *testing.T) {
	s := NewSession(1)
	s.maxPagesPerMatch = 1 // one block: every roll recycles the same buffer
	s.pageSize = 200
	// QueueDataSub is what marks the ack started, which is what makes PopAckTick
	// build and commit a frame instead of returning early.
	s.Ack.QueueDataSub(1, storeFrame(43, 0x01))
	// One tick creates the store, so the resend below can read its base without
	// racing the creation.
	s.PopAckTick()
	store := s.replay

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.PopAckTick()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// From the oldest RETAINED frame, which is the range a reconnect asks
			// for and the one the store compacts when it drops a block. Asking
			// from 0 would be refused as soon as the first block went, and a
			// refused resend copies nothing and so races with nothing.
			frames, ok := s.ResendFrames(store.Base())
			if !ok {
				continue
			}
			for _, f := range frames {
				if len(f.Body) == 0 {
					t.Error("a resent frame has an empty body")
					return
				}
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
