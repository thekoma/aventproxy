package tuya

import (
	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/utils"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type MQTTClient struct {
	mqtt           mqtt.Client
	uid            string
	subscribeTopic string

	// The broker delivers messages on its own goroutine, so the session map
	// is read there while streams add and remove sessions elsewhere.
	mu      sync.RWMutex
	cameras map[string]*MQTTCameraClient // sessionId -> camera
	imm     map[string]*IMMSession       // sessionId -> imm session

	// closed is also set by the broker's goroutine, from onDisconnect.
	closed    atomic.Bool
	Connected utils.Waiter
}

type MqttFrameHeader struct {
	Type          string `json:"type"`
	From          string `json:"from"`
	To            string `json:"to"`
	SubDevID      string `json:"sub_dev_id"`
	SessionID     string `json:"sessionid"`
	MotoID        string `json:"moto_id"`
	TransactionID string `json:"tid"`
	Seq           int    `json:"seq"`
	Rtx           int    `json:"rtx"`
}

type MqttFrame struct {
	Header  MqttFrameHeader `json:"header"`
	Message json.RawMessage `json:"msg"`
}

type MqttMessage struct {
	Protocol int       `json:"protocol"`
	Pv       string    `json:"pv"`
	T        int64     `json:"t"`
	Data     MqttFrame `json:"data"`
}

func NewMqttClient(clientId, mobileMqttsUrl string, mqttConfig *MQTConfig) (*MQTTClient, error) {
	client := &MQTTClient{
		uid:            mqttConfig.Msid,
		subscribeTopic: fmt.Sprintf("/av/u/%s", mqttConfig.Msid),
		Connected:      utils.Waiter{},
	}

	wssUrl := fmt.Sprintf("wss://%s/mqtt", mobileMqttsUrl)
	username := fmt.Sprintf("web_%s", mqttConfig.Msid)
	password := mqttConfig.Password

	opts := mqtt.NewClientOptions()
	opts.AddBroker(wssUrl)
	opts.SetClientID(username)
	opts.SetUsername(username)
	opts.SetPassword(password)
	opts.SetOnConnectHandler(client.onConnect)
	opts.SetConnectionLostHandler(client.onDisconnect)
	opts.SetAutoReconnect(false)
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetCleanSession(true)

	client.mqtt = mqtt.NewClient(opts)
	if token := client.mqtt.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	return client, nil
}

type MobileMQTTConfig struct {
	Username       string
	Password       string
	ClientID       string
	SubscribeTopic string
	UID            string
	BrokerURL      string
}

func NewMobileMqttClient(config *MobileMQTTConfig) (*MQTTClient, error) {
	client := &MQTTClient{
		uid:            config.UID,
		subscribeTopic: config.SubscribeTopic,
		Connected:      utils.Waiter{},
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(config.BrokerURL)
	opts.SetClientID(config.ClientID)
	opts.SetUsername(config.Username)
	opts.SetPassword(config.Password)
	opts.SetOnConnectHandler(client.onConnect)
	opts.SetConnectionLostHandler(client.onDisconnect)
	opts.SetAutoReconnect(false)
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetCleanSession(true)

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	opts.SetTLSConfig(tlsConfig)

	client.mqtt = mqtt.NewClient(opts)
	if token := client.mqtt.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	return client, nil
}

func (c *MQTTClient) Stop() {
	if c.mqtt != nil {
		c.mqtt.Disconnect(250)
		c.closed.Store(true)
	}
}

// UID is the account id this client subscribed with: its frames carry it as
// "from", and the monitor answers to it.
func (c *MQTTClient) UID() string { return c.uid }

func (c *MQTTClient) IsConnected() bool {
	return c.mqtt != nil && c.mqtt.IsConnected() && !c.closed.Load()
}

func (c *MQTTClient) AddCameraClient(sessionId string, cameraClient *MQTTCameraClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cameras == nil {
		c.cameras = make(map[string]*MQTTCameraClient)
	}
	c.cameras[sessionId] = cameraClient
}

func (c *MQTTClient) RemoveCameraClient(sessionId string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cameras, sessionId)
}

func (c *MQTTClient) cameraClient(sessionId string) (*MQTTCameraClient, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cameraClient, ok := c.cameras[sessionId]
	return cameraClient, ok
}

func (c *MQTTClient) onConnect(client mqtt.Client) {
	core.Logger.Trace().Msgf("Connected to mqtt broker")
	core.Logger.Trace().Msgf("Subscribing to topic: %s", c.subscribeTopic)

	if token := client.Subscribe(c.subscribeTopic, 1, c.consume); token.Wait() && token.Error() != nil {
		c.Connected.Done(token.Error())
		return
	}

	core.Logger.Trace().Msgf("Subscribed")
	c.Connected.Done(nil)
}

func (c *MQTTClient) onDisconnect(client mqtt.Client, err error) {
	if err != nil {
		c.Connected.Done(err)
	} else {
		c.Connected.Done(errors.New("mqtt client disconnected"))
	}

	c.closed.Store(true)
}

func (c *MQTTClient) consume(client mqtt.Client, msg mqtt.Message) {
	var rmqtt MqttMessage
	if err := json.Unmarshal(msg.Payload(), &rmqtt); err != nil {
		core.Logger.Error().Err(err).Msg("Failed to unmarshal mqtt message")
		return
	}

	sessionId := rmqtt.Data.Header.SessionID
	if s, ok := c.immSession(sessionId); ok {
		s.deliver(msg.Payload())
		return
	}
	cameraClient, ok := c.cameraClient(sessionId)
	if !ok {
		core.Logger.Warn().Msgf("No camera client found for sessionId: %s", sessionId)
		return
	}

	switch rmqtt.Data.Header.Type {
	case "answer":
		cameraClient.onMqttAnswer(&rmqtt)
	case "candidate":
		cameraClient.onMqttCandidate(&rmqtt)
	case "disconnect":
		cameraClient.onMqttDisconnect()
	}
}
