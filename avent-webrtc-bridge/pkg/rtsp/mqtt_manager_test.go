package rtsp

import (
	"sync/atomic"
	"testing"

	"avent-webrtc-bridge/pkg/tuya"
)

// fakeMQTT makes m connect without a broker, to connections that are up while
// up says so, and counts the connects.
func fakeMQTT(m *MQTTManager, up *atomic.Bool) *atomic.Int32 {
	var connects atomic.Int32
	m.connectFn = func(string) (*tuya.MQTTClient, error) {
		connects.Add(1)
		return &tuya.MQTTClient{}, nil
	}
	m.connected = func(*tuya.MQTTClient) bool { return up.Load() }
	return &connects
}

// Every connection authenticates with the same client id, and a broker drops
// the older connection when another arrives with its id (MQTT 3.1.1, 3.1.4-2).
// One connection per camera made each camera's connect cut the other's off,
// in the middle of its offer and answer if it was starting: the cameras share
// one, as the app's devices do.
func TestCamerasShareOneMQTTConnection(t *testing.T) {
	m := NewMQTTManager(nil)
	up := new(atomic.Bool)
	up.Store(true)
	connects := fakeMQTT(m, up)

	a, err := m.GetClient("cam1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.GetClient("cam2")
	if err != nil {
		t.Fatal(err)
	}
	if a != b || connects.Load() != 1 {
		t.Fatalf("two cameras got %d connections (same=%v), want one shared", connects.Load(), a == b)
	}
}

func TestSharedMQTTConnectionIsReplacedOnceDown(t *testing.T) {
	m := NewMQTTManager(nil)
	up := new(atomic.Bool)
	up.Store(true)
	connects := fakeMQTT(m, up)

	first, _ := m.GetClient("cam1")
	up.Store(false) // the broker dropped it
	second, _ := m.GetClient("cam2")
	up.Store(true)
	third, _ := m.GetClient("cam1")
	if first == second || second != third || connects.Load() != 2 {
		t.Fatalf("connects=%d first==second %v second==third %v; want a new shared connection once the old one is down",
			connects.Load(), first == second, second == third)
	}
}
