package tuya

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type MobileSDKClient struct {
	SigningKey       string
	SID              string
	AppKey           string
	DeviceID         string // phone device ID
	ChKey            string
	TTID             string
	BaseURL          string
	AppVersion       string
	SDKVersion       string
	Ecode            string
	PartnerIdentity  string
	UID              string
	PackageName      string
}

var signKeyWhitelist = []string{
	"a", "v", "lat", "lon", "lang", "deviceId", "appVersion", "ttid",
	"isH5", "h5Token", "os", "clientId", "postData", "time", "requestId",
	"et", "n4h5", "sid", "chKey", "sp",
}

func NewMobileSDKClient(signingKey, sid, appKey, deviceID, chKey string) *MobileSDKClient {
	return &MobileSDKClient{
		SigningKey:  signingKey,
		SID:        sid,
		AppKey:     appKey,
		DeviceID:   deviceID,
		ChKey:      chKey,
		TTID:       fmt.Sprintf("sdk_international@%s", appKey),
		BaseURL:    "https://a1.tuyaeu.com/api.json",
		AppVersion: "1.8.0",
		SDKVersion: "6.7.0",
	}
}

func swapSignString(s string) string {
	if len(s) != 32 {
		return s
	}
	return s[8:16] + s[0:8] + s[24:32] + s[16:24]
}

func (c *MobileSDKClient) sign(params map[string]string) string {
	whiteset := make(map[string]bool)
	for _, k := range signKeyWhitelist {
		whiteset[k] = true
	}

	filtered := make(map[string]string)
	for k, v := range params {
		if whiteset[k] && v != "" {
			filtered[k] = v
		}
	}

	if pd, ok := filtered["postData"]; ok && pd != "" {
		h := md5.Sum([]byte(pd))
		filtered["postData"] = swapSignString(hex.EncodeToString(h[:]))
	}

	keys := make([]string, 0, len(filtered))
	for k := range filtered {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + filtered[k]
	}
	signStr := strings.Join(parts, "||")

	mac := hmac.New(sha256.New, []byte(c.SigningKey))
	mac.Write([]byte(signStr))
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *MobileSDKClient) buildParams(action, version string, postData interface{}) map[string]string {
	t := fmt.Sprintf("%d", time.Now().Unix())
	params := map[string]string{
		"a":                action,
		"v":                version,
		"time":             t,
		"appVersion":       c.AppVersion,
		"appRnVersion":     "5.92",
		"channel":          "oem",
		"chKey":            c.ChKey,
		"clientId":         c.AppKey,
		"cp":               "gzip",
		"deviceCoreVersion": c.SDKVersion,
		"deviceId":         c.DeviceID,
		"et":               "0.0.1",
		"nd":               "1",
		"lang":             "en_US",
		"os":               "Android",
		"osSystem":         "14",
		"platform":         "tuya_bridge",
		"requestId":        uuid.New().String(),
		"sdkVersion":       c.SDKVersion,
		"sid":              c.SID,
		"timeZoneId":       "Europe/Rome",
		"ttid":             c.TTID,
	}

	if postData != nil {
		var pdStr string
		switch v := postData.(type) {
		case string:
			pdStr = v
		default:
			b, _ := json.Marshal(v)
			pdStr = string(b)
		}
		params["postData"] = pdStr
	}

	params["sign"] = c.sign(params)
	return params
}

func (c *MobileSDKClient) Call(action, version string, postData interface{}) (json.RawMessage, error) {
	params := c.buildParams(action, version, postData)

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	req, err := http.NewRequest("POST", c.BaseURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", fmt.Sprintf("Thing-UA=APP/Android/%s/SDK/%s", c.AppVersion, c.SDKVersion))

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Result   json.RawMessage `json:"result"`
		Success  bool            `json:"success"`
		ErrorMsg string          `json:"errorMsg,omitempty"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("JSON decode error: %v, body: %s", err, string(body[:200]))
	}
	if !result.Success {
		return nil, fmt.Errorf("API error: %s", result.ErrorMsg)
	}
	return result.Result, nil
}

func (c *MobileSDKClient) P2PPreLink() error {
	_, err := c.Call("thing.m.p2p.main.pre.link.get", "1.0", nil)
	return err
}

func (c *MobileSDKClient) RTCSessionInit(deviceID string) error {
	_, err := c.Call("smartlife.m.rtc.session.init", "1.0", map[string]string{"devId": deviceID})
	return err
}

func (c *MobileSDKClient) GetWebRTCConfig(deviceID string) (*WebRTCConfigResponse, error) {
	raw, err := c.Call("smartlife.m.rtc.config.get", "1.0", map[string]string{"devId": deviceID})
	if err != nil {
		return nil, err
	}
	var config WebRTCConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	return &WebRTCConfigResponse{Result: config, Success: true}, nil
}

func (c *MobileSDKClient) DeriveMQTTConfig(ecode string) *MQTConfig {
	md5SignKey := fmt.Sprintf("%x", md5.Sum([]byte(c.SigningKey)))
	pwFull := fmt.Sprintf("%x", md5.Sum([]byte(md5SignKey+ecode)))
	password := pwFull[8:24]
	md5AppKey := fmt.Sprintf("%x", md5.Sum([]byte(c.AppKey)))
	userTail := fmt.Sprintf("%x", md5.Sum([]byte(md5AppKey+ecode)))
	msid := userTail[len(userTail)-16:]
	return &MQTConfig{Msid: msid, Password: password}
}

func (c *MobileSDKClient) DeriveMQTTUsername(sid, ecode, partnerIdentity string) string {
	md5AppKey := fmt.Sprintf("%x", md5.Sum([]byte(c.AppKey)))
	userTail := fmt.Sprintf("%x", md5.Sum([]byte(md5AppKey+ecode)))
	return fmt.Sprintf("%s_v1_%s_%s_mb_%s%s",
		partnerIdentity, c.AppKey, c.ChKey, sid, userTail[len(userTail)-16:])
}

func (c *MobileSDKClient) DeriveMQTTClientID(uid string) string {
	uidHash := fmt.Sprintf("%x", md5.Sum([]byte(uid+"sdkfasodifca")))
	pkg := c.PackageName
	if pkg == "" {
		pkg = "tuya_bridge"
	}
	return fmt.Sprintf("%s_mb_%s_%s_DEFAULT", pkg, c.DeviceID, uidHash)
}

func (c *MobileSDKClient) GetUserInfo() (*UserInfoResult, error) {
	raw, err := c.Call("smartlife.m.user.info.get", "1.0", nil)
	if err != nil {
		return nil, err
	}
	var info UserInfoResult
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *MobileSDKClient) GetDeviceInfo(deviceID string) (json.RawMessage, error) {
	return c.Call("tuya.m.device.get", "1.0", map[string]string{"devId": deviceID})
}

type UserInfoResult struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Nickname   string `json:"nickname"`
	TimezoneId string `json:"timezoneId"`
	Domain     Domain `json:"domain"`
}
