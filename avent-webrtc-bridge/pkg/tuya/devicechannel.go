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

// The device channel is how the Philips app sends imm signalling: protocol-302
// frames out on smart/mb/out/<devId>, the device's answers in on
// smart/mb/in/<devId>, both framed with the device's local key
// (deviceframe.go).
//
// The bridge keeps one device-channel connection per account, shared by every
// camera that uses it, with one subscription per camera, as the app keeps one
// connection per tag for all its devices. It is a connection of its own, with
// a client id of its own (the shared webrtc connection's with another tag), so
// a broker that dislikes a publish on these topics can only drop this one,
// never the connection every camera's webrtc session shares. One per account
// and not one per camera, because a broker drops the older connection when a
// newer one arrives under the same client id. It closes when the last session
// lets go of it.

// DeviceChannelConfig names the broker, the identity and the device.
type DeviceChannelConfig struct {
	MQTT     MobileMQTTConfig // ClientID must differ from the shared connection's
	DeviceID string
	LocalKey string
	PV       string                // the device's pv; see UseDevicePV
	OnData   func(json.RawMessage) // protocol-302 data objects, on the broker's goroutine
}

// deviceChannelTimeout bounds the connect, the subscription and a release's
// unsubscribe.
var deviceChannelTimeout = 5 * time.Second

func deviceTopics(deviceID string) (in, out string) {
	return "smart/mb/in/" + deviceID, "smart/mb/out/" + deviceID
}

// DeviceChannels shares device-channel connections, one per client id, that
// is, one per account.
type DeviceChannels struct {
	// openMu serialises the network steps that change a connection's shape:
	// dialling, subscribing, and the deferred unsubscribe and disconnect. It is
	// never taken where a broker callback runs.
	openMu sync.Mutex
	// mu guards the maps and counts below. It is held only briefly, never
	// across a network wait, because route takes it on the broker's goroutine.
	mu    sync.Mutex
	conns map[string]*deviceConn

	dial func(cfg MobileMQTTConfig, onLost mqtt.ConnectionLostHandler) (mqtt.Client, error) // test seam
}

// DeviceChannelPool is the process's device-channel connections.
var DeviceChannelPool = NewDeviceChannels()

// NewDeviceChannels makes an empty pool.
func NewDeviceChannels() *DeviceChannels {
	return &DeviceChannels{conns: map[string]*deviceConn{}, dial: dialDeviceMQTT}
}

// deviceConn is one account's connection. Its fields are guarded by the
// pool's mu.
type deviceConn struct {
	id      string
	client  mqtt.Client
	refs    int // sessions holding it
	devices map[string]*deviceSubs
	lost    bool // dropped by the broker; no longer in the pool
	retired bool // let go by its last session; no longer in the pool
}

// deviceSubs is one subscribed device and the sessions reading it.
type deviceSubs struct {
	localKey []byte // the latest session's: a rotated key wins
	sessions map[*DeviceChannel]struct{}
}

// DeviceChannel is one session's use of its account's device channel.
type DeviceChannel struct {
	pool     *DeviceChannels
	conn     *deviceConn
	deviceID string
	localKey []byte
	pv       string // "2.2" or "2.3"
	onData   func(json.RawMessage)

	mu     sync.Mutex
	header DeviceFrameHeader
	closed bool
}

// newDeviceChannel prepares a session's state; Open connects it.
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

// DialDeviceChannel opens a session on the process's shared pool.
func DialDeviceChannel(cfg DeviceChannelConfig) (*DeviceChannel, error) {
	return DeviceChannelPool.Open(cfg)
}

// Open returns a session on the account's connection, dialling it if there is
// none, and subscribing to the device if no other session reads it yet.
func (p *DeviceChannels) Open(cfg DeviceChannelConfig) (*DeviceChannel, error) {
	if len(cfg.LocalKey) != 16 {
		return nil, errors.New("device channel: the local key must be 16 bytes")
	}
	d := newDeviceChannel(cfg)
	d.pool = p
	id := cfg.MQTT.ClientID

	p.openMu.Lock()
	defer p.openMu.Unlock()

	p.mu.Lock()
	c := p.conns[id]
	p.mu.Unlock()
	if c == nil {
		conn := &deviceConn{id: id, devices: map[string]*deviceSubs{}}
		client, err := p.dial(cfg.MQTT, func(_ mqtt.Client, err error) { p.lost(conn, err) })
		if err != nil {
			return nil, err
		}
		conn.client = client
		p.mu.Lock()
		p.conns[id] = conn
		p.mu.Unlock()
		c = conn
	}

	p.mu.Lock()
	subs := c.devices[cfg.DeviceID]
	p.mu.Unlock()
	if subs == nil {
		in, _ := deviceTopics(cfg.DeviceID)
		devID := cfg.DeviceID
		token := c.client.Subscribe(in, 0, func(_ mqtt.Client, msg mqtt.Message) { p.route(c, devID, msg) }) // QoS 0, as the app subscribes
		var err error
		switch {
		case !token.WaitTimeout(deviceChannelTimeout):
			err = errors.New("device channel: subscribe timed out")
		case token.Error() != nil:
			err = fmt.Errorf("device channel: subscribe: %w", token.Error())
		}
		if err != nil {
			p.retireIfUnused(c)
			return nil, err
		}
		subs = &deviceSubs{sessions: map[*DeviceChannel]struct{}{}}
		p.mu.Lock()
		c.devices[cfg.DeviceID] = subs
		p.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if c.lost || c.retired {
		return nil, errors.New("device channel: the connection dropped while opening")
	}
	subs.localKey = d.localKey
	subs.sessions[d] = struct{}{}
	c.refs++
	d.conn = c
	return d, nil
}

// retireIfUnused drops a connection no session holds, after a failed open.
// Callers hold openMu.
func (p *DeviceChannels) retireIfUnused(c *deviceConn) {
	p.mu.Lock()
	unused := c.refs == 0 && len(c.devices) == 0 && !c.retired
	if unused {
		c.retired = true
		if p.conns[c.id] == c {
			delete(p.conns, c.id)
		}
	}
	p.mu.Unlock()
	if unused {
		c.client.Disconnect(250)
	}
}

// lost is the connection-lost handler: the pool forgets the connection, so
// the next session dials a new one. Sessions holding it keep their handle;
// their publishes fail until they end.
func (p *DeviceChannels) lost(c *deviceConn, err error) {
	p.mu.Lock()
	c.lost = true
	if p.conns[c.id] == c {
		delete(p.conns, c.id)
	}
	p.mu.Unlock()
	core.Logger.Info().Msgf("Device channel lost: %v", err)
}

// route hands one inbound frame to the sessions reading its device. It runs
// on the broker's goroutine and takes mu only briefly.
func (p *DeviceChannels) route(c *deviceConn, deviceID string, msg mqtt.Message) {
	p.mu.Lock()
	subs := c.devices[deviceID]
	var key []byte
	var readers []*DeviceChannel
	if subs != nil {
		key = subs.localKey
		for s := range subs.sessions {
			readers = append(readers, s)
		}
	}
	p.mu.Unlock()
	if len(readers) == 0 {
		return
	}
	protocol, data, err := DecodeDeviceFrame(key, msg.Payload())
	if err != nil {
		// A frame this key cannot open says something (a rotated local key,
		// a framing guess gone wrong); the error never carries the key.
		core.Logger.Debug().Err(err).Msgf("Unreadable frame on the device channel of %s (%d bytes)", deviceID, len(msg.Payload()))
		return
	}
	if protocol != 302 || len(data) == 0 {
		return
	}
	for _, s := range readers {
		if s.onData != nil {
			s.onData(data)
		}
	}
}

// Publish sends one protocol-302 data object ({"header":…,"msg":…}) to the
// device. QoS 0 and not retained, as the app's send302Message does.
func (d *DeviceChannel) Publish(frame any) error {
	return d.PublishWithin(frame, publishTimeout)
}

// PublishWithin is Publish with its own bound on the wait for the write.
func (d *DeviceChannel) PublishWithin(frame any, wait time.Duration) error {
	d.mu.Lock()
	d.header.S++
	h := d.header
	d.mu.Unlock()
	payload, err := EncodeDeviceFrame(d.pv, d.localKey, h, 302, time.Now().Unix(), frame)
	if err != nil {
		return err
	}
	_, out := deviceTopics(d.deviceID)
	token := d.conn.client.Publish(out, 0, false, payload)
	if !token.WaitTimeout(wait) {
		return errPublishTimeout
	}
	return token.Error()
}

// Close lets go of the session. It never waits on the network: the
// unsubscribe (when no other session reads the device) and the disconnect
// (when no other session holds the connection) run on their own goroutine.
func (d *DeviceChannel) Close() {
	d.mu.Lock()
	if d.closed || d.conn == nil {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.mu.Unlock()
	d.pool.release(d)
}

func (p *DeviceChannels) release(d *DeviceChannel) {
	c := d.conn
	p.mu.Lock()
	c.refs--
	if subs := c.devices[d.deviceID]; subs != nil {
		delete(subs.sessions, d)
		if len(subs.sessions) == 0 {
			delete(c.devices, d.deviceID)
			go p.unsubscribe(c, d.deviceID)
		}
	}
	if c.refs == 0 {
		go p.retire(c)
	}
	p.mu.Unlock()
}

// unsubscribe drops a device nobody reads any more, unless a session opened
// it again meanwhile: openMu orders this after any subscribe in flight.
func (p *DeviceChannels) unsubscribe(c *deviceConn, deviceID string) {
	p.openMu.Lock()
	defer p.openMu.Unlock()
	p.mu.Lock()
	again := c.devices[deviceID] != nil
	gone := c.lost || c.retired
	p.mu.Unlock()
	if again || gone {
		return
	}
	in, _ := deviceTopics(deviceID)
	c.client.Unsubscribe(in).WaitTimeout(deviceChannelTimeout)
}

// retire closes a connection no session holds, unless one took it again.
func (p *DeviceChannels) retire(c *deviceConn) {
	p.openMu.Lock()
	defer p.openMu.Unlock()
	p.mu.Lock()
	if c.refs > 0 || c.retired {
		p.mu.Unlock()
		return
	}
	c.retired = true
	if p.conns[c.id] == c {
		delete(p.conns, c.id)
	}
	p.mu.Unlock()
	c.client.Disconnect(250)
}

// dialDeviceMQTT connects a device-channel connection.
func dialDeviceMQTT(cfg MobileMQTTConfig, onLost mqtt.ConnectionLostHandler) (mqtt.Client, error) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.BrokerURL)
	opts.SetClientID(cfg.ClientID)
	opts.SetUsername(cfg.Username)
	opts.SetPassword(cfg.Password)
	opts.SetAutoReconnect(false)
	opts.SetConnectTimeout(deviceChannelTimeout)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetCleanSession(true)
	opts.SetConnectionLostHandler(onLost)
	opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(deviceChannelTimeout) {
		client.Disconnect(0)
		return nil, errors.New("device channel: connect timed out")
	}
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("device channel: connect: %w", err)
	}
	return client, nil
}
