package rtsp

import (
	"fmt"
	"sync"
	"time"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/tuya"
)

var mqttBackoffDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

// mqttHandshakeTimeout bounds the subscription that follows a connect. It runs
// with the stream's lock held, so a broker that never acknowledges must not
// keep the stream waiting for good.
const mqttHandshakeTimeout = 15 * time.Second

// MQTTManager holds the one MQTT connection the cameras' cloud signalling
// shares. One, not one per camera: every connection authenticates with the
// same client id (derived from the phone's device id and the user's), and a
// broker drops the older connection when another arrives with its id (MQTT
// 3.1.1, 3.1.4-2). With two cameras, each camera's connect cut the other's
// off, in the middle of its offer and answer if it was starting. The app
// keeps one connection for all its devices too; sessions are told apart by
// their session id (MQTTClient.AddCameraClient).
type MQTTManager struct {
	mobileClient *tuya.MobileSDKClient
	client       *tuya.MQTTClient
	mutex        sync.Mutex

	// Seams for tests: connect, and (*tuya.MQTTClient).IsConnected.
	connectFn func(deviceId string) (*tuya.MQTTClient, error)
	connected func(*tuya.MQTTClient) bool
}

func NewMQTTManager(mobileClient *tuya.MobileSDKClient) *MQTTManager {
	m := &MQTTManager{
		mobileClient: mobileClient,
		connected:    (*tuya.MQTTClient).IsConnected,
	}
	m.connectFn = m.connect
	return m
}

// GetClient returns the shared connection, connecting it first if it is down.
// deviceId is the camera that needs it, for the log.
func (m *MQTTManager) GetClient(deviceId string) (*tuya.MQTTClient, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.client != nil && m.connected(m.client) {
		core.Logger.Trace().Msgf("Reusing existing MQTT client for device %s", deviceId)
		return m.client, nil
	}

	core.Logger.Info().Msgf("Creating new MQTT client for device %s", deviceId)
	client, err := m.connectFn(deviceId)
	if err != nil {
		return nil, err
	}

	m.client = client
	return client, nil
}

func (m *MQTTManager) connect(deviceId string) (*tuya.MQTTClient, error) {
	if m.mobileClient == nil {
		return nil, fmt.Errorf("mobile client not configured")
	}

	userInfo, err := m.mobileClient.GetUserInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %v", err)
	}

	ecode := m.mobileClient.Ecode
	// The cloud's user id, not the configured uid that LAN offers carry.
	uid := userInfo.ID
	if uid == "" {
		uid = m.mobileClient.UID
	}
	mqttDerived := m.mobileClient.DeriveMQTTConfig(ecode)
	mqttUsername := m.mobileClient.DeriveMQTTUsername(m.mobileClient.SID, ecode, m.mobileClient.PartnerIdentity)
	mqttClientID := m.mobileClient.DeriveMQTTClientID(uid)
	subscribeTopic := fmt.Sprintf("/av/u/%s", uid)
	brokerURL := fmt.Sprintf("ssl://%s:8883", userInfo.Domain.MobileMqttsUrl)

	var lastErr error
	for attempt, delay := range mqttBackoffDelays {
		if attempt > 0 {
			core.Logger.Info().Msgf("MQTT connect retry %d/%d for device %s in %v", attempt+1, len(mqttBackoffDelays), deviceId, delay)
			time.Sleep(delay)
		}

		client, err := tuya.NewMobileMqttClient(&tuya.MobileMQTTConfig{
			Username:       mqttUsername,
			Password:       mqttDerived.Password,
			ClientID:       mqttClientID,
			SubscribeTopic: subscribeTopic,
			UID:            uid,
			BrokerURL:      brokerURL,
		})
		if err != nil {
			lastErr = err
			core.Logger.Warn().Err(err).Msgf("MQTT connect attempt %d/%d failed for device %s", attempt+1, len(mqttBackoffDelays), deviceId)
			continue
		}

		if err = client.Connected.WaitTimeout(mqttHandshakeTimeout); err != nil {
			lastErr = err
			client.Stop()
			core.Logger.Warn().Err(err).Msgf("MQTT handshake attempt %d/%d failed for device %s", attempt+1, len(mqttBackoffDelays), deviceId)
			continue
		}

		core.Logger.Info().Msgf("MQTT connected for device %s", deviceId)
		return client, nil
	}

	return nil, fmt.Errorf("MQTT connect failed after %d attempts: %v", len(mqttBackoffDelays), lastErr)
}

// InvalidateClient drops the shared connection, so the next GetClient
// connects afresh. deviceId is the camera that found it broken, for the log.
func (m *MQTTManager) InvalidateClient(deviceId string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.client != nil {
		core.Logger.Info().Msgf("Invalidating MQTT client for device %s (will reconnect on next use)", deviceId)
		m.client.Stop()
		m.client = nil
	}
}

func (m *MQTTManager) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.client != nil {
		core.Logger.Info().Msg("Closing the MQTT client")
		m.client.Stop()
		m.client = nil
	}
}
