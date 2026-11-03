package tuya

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"avent-webrtc-bridge/pkg/core"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// DeviceChannel is a short-lived MQTT connection that speaks to one device
// on its own channel, the way the Philips app sends imm signalling:
// protocol-302 frames out on smart/mb/out/<devId>, the device's answers in on
// smart/mb/in/<devId>, both framed with the device's local key (deviceframe.go).
//
// It is a connection of its own, with a client id of its own, so that a broker
// that dislikes a publish on this topic can only drop this connection, never
// the one every camera's webrtc session shares. The app runs several
// connections per account the same way, one per tag.
type DeviceChannel struct {
	client   mqtt.Client
	deviceID string
	localKey []byte
	pv       string // "2.2" or "2.3"
	onData   func(json.RawMessage)

	mu     sync.Mutex
	header DeviceFrameHeader
}

// DeviceChannelConfig names the broker, the identity and the device.
type DeviceChannelConfig struct {
	MQTT     MobileMQTTConfig // ClientID must differ from the shared connection's
	DeviceID string
	LocalKey string
	PV       string                // the device's pv; see UseDevicePV
	OnData   func(json.RawMessage) // protocol-302 data objects, on the broker's goroutine
}

// deviceChannelTimeout bounds the connect and the subscription.
var deviceChannelTimeout = 5 * time.Second

func deviceTopics(deviceID string) (in, out string) {
	return "smart/mb/in/" + deviceID, "smart/mb/out/" + deviceID
}

// newDeviceChannel prepares the channel's state; DialDeviceChannel connects it.
func newDeviceChannel(cfg DeviceChannelConfig) *DeviceChannel {
	return &DeviceChannel{
		deviceID: cfg.DeviceID,
		localKey: []byte(cfg.LocalKey),
		pv:       UseDevicePV(cfg.PV),
		onData:   cfg.OnData,
		// SandO: the app's counter starts at 2 and is incremented before each
		// publish; its other half is random in [1000, 1001000).
		header: DeviceFrameHeader{S: 2, O: uint32(1000 + rand.IntN(1000000))},
	}
}

// DialDeviceChannel connects and subscribes to the device's inbound topic.
func DialDeviceChannel(cfg DeviceChannelConfig) (*DeviceChannel, error) {
	if len(cfg.LocalKey) != 16 {
		return nil, errors.New("device channel: the local key must be 16 bytes")
	}
	d := newDeviceChannel(cfg)
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.BrokerURL)
	opts.SetClientID(cfg.MQTT.ClientID)
	opts.SetUsername(cfg.MQTT.Username)
	opts.SetPassword(cfg.MQTT.Password)
	opts.SetAutoReconnect(false)
	opts.SetConnectTimeout(deviceChannelTimeout)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetCleanSession(true)
	opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	d.client = mqtt.NewClient(opts)

	token := d.client.Connect()
	if !token.WaitTimeout(deviceChannelTimeout) {
		d.client.Disconnect(0)
		return nil, errors.New("device channel: connect timed out")
	}
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("device channel: connect: %w", err)
	}
	in, _ := deviceTopics(cfg.DeviceID)
	sub := d.client.Subscribe(in, 0, d.receive) // QoS 0, as the app subscribes
	if !sub.WaitTimeout(deviceChannelTimeout) || sub.Error() != nil {
		d.client.Disconnect(0)
		return nil, fmt.Errorf("device channel: subscribe: %v", sub.Error())
	}
	return d, nil
}

// Publish sends one protocol-302 data object ({"header":…,"msg":…}) to the
// device. QoS 0 and not retained, as the app's send302Message does.
func (d *DeviceChannel) Publish(frame any) error {
	d.mu.Lock()
	d.header.S++
	h := d.header
	d.mu.Unlock()
	payload, err := EncodeDeviceFrame(d.pv, d.localKey, h, 302, time.Now().Unix(), frame)
	if err != nil {
		return err
	}
	_, out := deviceTopics(d.deviceID)
	token := d.client.Publish(out, 0, false, payload)
	if !token.WaitTimeout(publishTimeout) {
		return errPublishTimeout
	}
	return token.Error()
}

// Close drops the connection.
func (d *DeviceChannel) Close() {
	if d.client != nil {
		d.client.Disconnect(250)
	}
}

// receive hands the device's protocol-302 data objects to onData. Anything
// else on the topic (status pushes, frames it cannot read) is dropped.
func (d *DeviceChannel) receive(_ mqtt.Client, msg mqtt.Message) {
	protocol, data, err := DecodeDeviceFrame(d.localKey, msg.Payload())
	if err != nil {
		// A frame this key cannot open says something (a rotated local key,
		// a framing guess gone wrong); the error never carries the key.
		core.Logger.Debug().Err(err).Msgf("Unreadable frame on the device channel of %s (%d bytes)", d.deviceID, len(msg.Payload()))
		return
	}
	if protocol != 302 || len(data) == 0 || d.onData == nil {
		return
	}
	d.onData(data)
}
