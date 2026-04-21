package rtsp

import (
	"fmt"
	"sync"
	"time"

	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/tuya"
)

var mqttBackoffDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

type MQTTManager struct {
	mobileClient *tuya.MobileSDKClient
	clients      map[string]*tuya.MQTTClient // keyed by deviceId
	mutex        sync.Mutex
}

func NewMQTTManager(mobileClient *tuya.MobileSDKClient) *MQTTManager {
	return &MQTTManager{
		mobileClient: mobileClient,
		clients:      make(map[string]*tuya.MQTTClient),
	}
}

func (m *MQTTManager) GetClient(deviceId string) (*tuya.MQTTClient, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if client, exists := m.clients[deviceId]; exists && client.IsConnected() {
		core.Logger.Trace().Msgf("Reusing existing MQTT client for device %s", deviceId)
		return client, nil
	}

	core.Logger.Info().Msgf("Creating new MQTT client for device %s", deviceId)
	client, err := m.connect(deviceId)
	if err != nil {
		return nil, err
	}

	m.clients[deviceId] = client
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
	uid := m.mobileClient.UID
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

		if err = client.Connected.Wait(); err != nil {
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

func (m *MQTTManager) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for deviceId, client := range m.clients {
		core.Logger.Info().Msgf("Closing MQTT client for device %s", deviceId)
		client.Stop()
	}
	m.clients = make(map[string]*tuya.MQTTClient)
}
