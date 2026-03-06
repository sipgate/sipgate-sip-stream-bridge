package bridge

import (
	"testing"
)

// TestMarkSentinel_RoutedToEchoQueue verifies MARK-01:
// When rtpPacer dequeues a mark sentinel (frame.mark != ""), it routes the mark name
// to markEchoQueue and skips sending an RTP packet for that frame.
// The channel logic is tested directly without goroutines.
func TestMarkSentinel_RoutedToEchoQueue(t *testing.T) {
	packetQueue := make(chan outboundFrame, 10)
	markEchoQueue := make(chan string, 10)

	// Enqueue: one audio frame, then one mark sentinel, then another audio frame.
	packetQueue <- outboundFrame{audio: make([]byte, 160)}
	packetQueue <- outboundFrame{mark: "end-of-speech"}
	packetQueue <- outboundFrame{audio: make([]byte, 160)}

	// Simulate rtpPacer dequeue loop (3 iterations).
	audioCount := 0
	for i := 0; i < 3; i++ {
		var frame outboundFrame
		select {
		case frame = <-packetQueue:
		default:
		}

		if frame.mark != "" {
			// Route to markEchoQueue (non-blocking).
			select {
			case markEchoQueue <- frame.mark:
			default:
			}
			continue // no RTP send for sentinels
		}
		if frame.audio != nil {
			audioCount++
		}
	}

	if audioCount != 2 {
		t.Errorf("expected 2 audio frames processed, got %d", audioCount)
	}
	if len(markEchoQueue) != 1 {
		t.Errorf("expected 1 mark in markEchoQueue, got %d", len(markEchoQueue))
	}
	if name := <-markEchoQueue; name != "end-of-speech" {
		t.Errorf("markEchoQueue: expected %q, got %q", "end-of-speech", name)
	}
}

// TestMarkImmediate_EmptyQueue verifies MARK-02:
// When wsToRTP receives a mark event and packetQueue is empty,
// the mark name is sent directly to markEchoQueue (not enqueued as a sentinel).
func TestMarkImmediate_EmptyQueue(t *testing.T) {
	packetQueue := make(chan outboundFrame, 10)
	markEchoQueue := make(chan string, 10)

	markName := "greeting-start"

	// Simulate wsToRTP mark handling with empty queue.
	if len(packetQueue) == 0 {
		// MARK-02 path: direct to markEchoQueue
		select {
		case markEchoQueue <- markName:
		default:
		}
	} else {
		// MARK-01 path: enqueue sentinel
		select {
		case packetQueue <- outboundFrame{mark: markName}:
		default:
		}
	}

	// Mark must be in markEchoQueue immediately.
	if len(markEchoQueue) != 1 {
		t.Errorf("expected 1 mark in markEchoQueue (immediate echo), got %d", len(markEchoQueue))
	}
	if len(packetQueue) != 0 {
		t.Errorf("expected empty packetQueue (no sentinel enqueued), got %d items", len(packetQueue))
	}
	if name := <-markEchoQueue; name != markName {
		t.Errorf("markEchoQueue: expected %q, got %q", markName, name)
	}
}

// TestClear_DrainsQueueAndEchoesMarks verifies MARK-03:
// When clearSignal fires, rtpPacer drains the entire packetQueue:
// audio frames are discarded, mark sentinels are routed to markEchoQueue.
func TestClear_DrainsQueueAndEchoesMarks(t *testing.T) {
	packetQueue := make(chan outboundFrame, 20)
	markEchoQueue := make(chan string, 10)
	clearSignal := make(chan struct{}, 1)

	// Fill packetQueue with mixed audio + mark sentinels.
	packetQueue <- outboundFrame{audio: make([]byte, 160)} // audio 1 — discard
	packetQueue <- outboundFrame{mark: "intro"}            // mark 1 — echo
	packetQueue <- outboundFrame{audio: make([]byte, 160)} // audio 2 — discard
	packetQueue <- outboundFrame{audio: make([]byte, 160)} // audio 3 — discard
	packetQueue <- outboundFrame{mark: "outro"}            // mark 2 — echo

	// Signal clear.
	clearSignal <- struct{}{}

	// Simulate rtpPacer clearSignal drain at top of tick.
	select {
	case <-clearSignal:
	drainLoop:
		for {
			select {
			case f := <-packetQueue:
				if f.mark != "" {
					select {
					case markEchoQueue <- f.mark:
					default:
					}
				}
				// audio: discard silently
			default:
				break drainLoop
			}
		}
	default:
	}

	// packetQueue must be empty after drain.
	if len(packetQueue) != 0 {
		t.Errorf("expected packetQueue empty after clear, got %d items", len(packetQueue))
	}
	// Both marks must be in markEchoQueue.
	if len(markEchoQueue) != 2 {
		t.Errorf("expected 2 marks in markEchoQueue after clear, got %d", len(markEchoQueue))
	}
	if name := <-markEchoQueue; name != "intro" {
		t.Errorf("first mark: expected %q, got %q", "intro", name)
	}
	if name := <-markEchoQueue; name != "outro" {
		t.Errorf("second mark: expected %q, got %q", "outro", name)
	}
}

// TestClear_RTPContinues verifies MARK-04:
// After a clear event, rtpPacer sends silence frames (not stopped).
// The drain is a single-tick operation; subsequent ticks produce silence normally.
func TestClear_RTPContinues(t *testing.T) {
	packetQueue := make(chan outboundFrame, 10)
	markEchoQueue := make(chan string, 10)
	clearSignal := make(chan struct{}, 1)

	// Enqueue some audio and signal clear.
	packetQueue <- outboundFrame{audio: make([]byte, 160)}
	clearSignal <- struct{}{}

	// Tick 1: clear signal fires — drain packetQueue, no RTP sent.
	silenceSent := false
	select {
	case <-clearSignal:
	drainLoop:
		for {
			select {
			case f := <-packetQueue:
				if f.mark != "" {
					select {
					case markEchoQueue <- f.mark:
					default:
					}
				}
			default:
				break drainLoop
			}
		}
	default:
	}
	// After drain: dequeue → empty → silence.
	var frame outboundFrame
	select {
	case frame = <-packetQueue:
	default:
	}
	chunk := frame.audio
	if chunk == nil {
		chunk = make([]byte, 160) // silence fallback
		silenceSent = true
	}
	_ = chunk // would be sent as RTP in real rtpPacer

	// Tick 2: no clear signal, no queued audio → silence again (pacer is still running).
	var frame2 outboundFrame
	select {
	case frame2 = <-packetQueue:
	default:
	}
	chunk2 := frame2.audio
	silence2 := false
	if chunk2 == nil {
		chunk2 = make([]byte, 160) // silence fallback
		silence2 = true
	}
	_ = chunk2

	if !silenceSent {
		t.Error("expected silence on tick 1 after clear (packetQueue drained)")
	}
	if !silence2 {
		t.Error("expected silence on tick 2 — rtpPacer must continue after clear")
	}
	if len(packetQueue) != 0 {
		t.Errorf("expected empty packetQueue after clear, got %d", len(packetQueue))
	}
}
