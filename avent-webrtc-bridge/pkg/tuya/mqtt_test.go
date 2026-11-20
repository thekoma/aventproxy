package tuya

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// fakeMessage is an inbound MQTT message carrying a 302 frame.
type fakeMessage struct {
	mqtt.Message
	payload []byte
}

func (m fakeMessage) Payload() []byte { return m.payload }

func disconnectFor(t *testing.T, sessionID string) mqtt.Message {
	t.Helper()
	b, err := json.Marshal(MqttMessage{Data: MqttFrame{Header: MqttFrameHeader{Type: "disconnect", SessionID: sessionID}}})
	if err != nil {
		t.Fatal(err)
	}
	return fakeMessage{payload: b}
}

// The broker delivers on its own goroutine while streams add and remove their
// sessions elsewhere; without a lock that is a concurrent map read and write,
// which ends the process (run with -race).
func TestSessionMapSurvivesConcurrentDelivery(t *testing.T) {
	c := &MQTTClient{}
	var wg sync.WaitGroup
	for i := range 4 {
		id := fmt.Sprintf("s%d", i)
		msg := disconnectFor(t, id)
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				c.AddCameraClient(id, &MQTTCameraClient{SessionId: id})
				c.RemoveCameraClient(id)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				c.consume(nil, msg)
			}
		}()
	}
	wg.Wait()
}

func TestDisconnectFlagIsSafeAcrossGoroutines(t *testing.T) {
	c := &MQTTClient{}
	cam := &MQTTCameraClient{mqttClient: c}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.onDisconnect(nil, errors.New("broker gone"))
	}()
	_ = c.IsConnected()
	<-done
	if err := cam.SendDisconnect(); err == nil {
		t.Fatal("a send on a disconnected client must fail")
	}
}

// stuckToken never completes, like a QoS 1 publish whose PUBACK never comes.
type stuckToken struct{ mqtt.Token }

func (stuckToken) Wait() bool                       { select {} }
func (stuckToken) WaitTimeout(d time.Duration) bool { time.Sleep(d); return false }
func (stuckToken) Done() <-chan struct{}            { return make(chan struct{}) }
func (stuckToken) Error() error                     { return nil }

type stuckBroker struct{ mqtt.Client }

func (stuckBroker) Publish(string, byte, bool, interface{}) mqtt.Token { return stuckToken{} }

// SendDisconnect runs under the stream's lock during teardown: a broker that
// never acknowledges must not hold that lock for good.
func TestPublishIsBounded(t *testing.T) {
	old := publishTimeout
	publishTimeout = 50 * time.Millisecond
	defer func() { publishTimeout = old }()

	cam := &MQTTCameraClient{mqttClient: &MQTTClient{mqtt: stuckBroker{}}}
	errc := make(chan error, 1)
	go func() { errc <- cam.SendDisconnect() }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("an unacknowledged publish must report an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendDisconnect still waiting for a broker that never answers")
	}
}
