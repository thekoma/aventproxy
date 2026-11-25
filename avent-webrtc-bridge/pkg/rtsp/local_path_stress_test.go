package rtsp

// One camera, an automatic and a local stream, random client arrivals and
// departures, stops, LAN deaths and cool-down flips (200 rounds, about 5 s).
// A session counts as open at the monitor from the fake's dial (Start entry)
// to the end of its Stop, which takes a while (a slow close). At most one may
// be open at once, and none, with the hold free, once both streams stop.

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"avent-webrtc-bridge/pkg/storage"
)

type slowCloseLAN struct {
	open, peak *atomic.Int32
	failStart  bool
	mu         sync.Mutex
	stopped    bool
	dialed     bool
	onErr      func(error)
}

func (f *slowCloseLAN) Start(ctx context.Context) error {
	f.mu.Lock()
	if f.stopped || f.dialed {
		f.mu.Unlock()
		return errors.New("used")
	}
	f.dialed = true
	n := f.open.Add(1)
	for m := f.peak.Load(); n > m && !f.peak.CompareAndSwap(m, n); m = f.peak.Load() {
	}
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(300 * time.Microsecond):
	}
	if f.failStart {
		return errors.New("fake LAN failure")
	}
	return nil
}

func (f *slowCloseLAN) Stop() {
	f.mu.Lock()
	first := !f.stopped
	f.stopped = true
	dialed := f.dialed
	f.mu.Unlock()
	if first && dialed {
		time.Sleep(500 * time.Microsecond) // the close takes a moment
		f.open.Add(-1)
	}
}

func (f *slowCloseLAN) SetOnError(fn func(error)) { f.mu.Lock(); f.onErr = fn; f.mu.Unlock() }

func (f *slowCloseLAN) die() {
	f.mu.Lock()
	fn, ok := f.onErr, f.dialed && !f.stopped
	f.mu.Unlock()
	if ok && fn != nil {
		fn(errors.New("fake death"))
	}
}

func TestLocalPathAtMostOneLANSessionUnderStress(t *testing.T) {
	var open, peak atomic.Int32
	for it := 0; it < 200; it++ {
		rng := rand.New(rand.NewSource(int64(it) * 7919))
		server := newLANServer()
		cam := &storage.CameraInfo{DeviceID: "testdev", DeviceName: "Test Cam"}
		var mu sync.Mutex
		var srcs []*slowCloseLAN
		frng := rand.New(rand.NewSource(int64(it)))
		mk := func(local bool) *CameraStream {
			cs := newCameraStream(cam, "hd", &storage.UserSession{UserKey: "u"}, nil, server, local)
			cs.streamId = "testdev-hd"
			if local {
				cs.streamId += "-local"
			}
			cs.attachBridgeErrorHandler()
			cs.SetShutdownDelay(time.Duration(rng.Intn(2)) * time.Millisecond)
			cs.lanFactory = func(*RTPForwarder) lanStreamer {
				mu.Lock()
				defer mu.Unlock()
				f := &slowCloseLAN{open: &open, peak: &peak, failStart: frng.Intn(6) == 0}
				srcs = append(srcs, f)
				return f
			}
			cs.cloudStart = func(ctx context.Context, _ *WebRTCBridge) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(200 * time.Microsecond):
				}
				return nil
			}
			return cs
		}
		streams := []*CameraStream{mk(false), mk(true)}
		var wg sync.WaitGroup
		var cmu sync.Mutex
		var conns []net.Conn
		for g := 0; g < 5; g++ {
			seed := rng.Int63()
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := rand.New(rand.NewSource(seed))
				for i := 0; i < 50; i++ {
					cs := streams[r.Intn(2)]
					switch r.Intn(9) {
					case 0, 1, 2:
						a, b := net.Pipe()
						cmu.Lock()
						conns = append(conns, a, b)
						cmu.Unlock()
						cs.AddClient(&RTSPClient{conn: b, session: strconv.Itoa(r.Int()), stream: cs})
					case 3:
						cs.mutex.RLock()
						var ids []string
						for id := range cs.clients {
							ids = append(ids, id)
						}
						cs.mutex.RUnlock()
						for _, id := range ids {
							cs.RemoveClient(id)
						}
					case 4:
						cs.stopStream()
					case 5:
						server.cooldownMutex.Lock()
						server.lanCooldowns["testdev"] = time.Now().Add(time.Duration(r.Intn(3)) * time.Millisecond)
						server.cooldownMutex.Unlock()
					case 6:
						mu.Lock()
						var f *slowCloseLAN
						if len(srcs) > 0 {
							f = srcs[r.Intn(len(srcs))]
						}
						mu.Unlock()
						if f != nil {
							f.die()
						}
					default:
						time.Sleep(time.Duration(r.Intn(1500)) * time.Microsecond)
					}
				}
			}()
		}
		ch := make(chan struct{})
		go func() { wg.Wait(); close(ch) }()
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Fatalf("iteration %d: ops did not finish", it)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			for _, cs := range streams {
				cs.Stop()
				cs.stopStream()
			}
			if open.Load() == 0 && server.lanHolders.heldBy("testdev") == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: %d sessions open, holder %v after both stopped", it, open.Load(), server.lanHolders.heldBy("testdev") != nil)
			}
			time.Sleep(5 * time.Millisecond)
		}
		cmu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		cmu.Unlock()
		if p := peak.Load(); p > 1 {
			t.Fatalf("iteration %d: %d LAN sessions open at once", it, p)
		}
	}
	t.Logf("peak %d", peak.Load())
}
