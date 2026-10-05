package dice

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	onebotBridgeDefaultBind = "0.0.0.0:18081"
	onebotBridgeSuffix      = "/ws"
)

// PreflightOnebotLLMBridge validates the dedicated volume before database
// initialization can create files in data/default. Call only when bridge mode
// is enabled, before DeleteOldWrongFile, GetDatabaseOperator, or migrations.
func PreflightOnebotLLMBridge(bind, token string) error {
	return preflightOnebotBridgeVolume(bind, token, "data/default", "data/packages", "cache/packages")
}

func preflightOnebotBridgeVolume(bind, token, dataDir, packageSourceDir, packageCacheDir string) error {
	if err := validateOnebotBridgeOptions(bind, token); err != nil {
		return err
	}
	return validateOnebotBridgeData(dataDir, packageSourceDir, packageCacheDir)
}

// ConfigureOnebotLLMBridge keeps runtime-only configuration on the one
// default Dice instance. Call after TryCreateDefault and before InitDice; the
// volume preflight must already have run before database initialization.
func ConfigureOnebotLLMBridge(dm *DiceManager, bind, token string) error {
	if dm == nil {
		return errors.New("bridge startup requires a Dice manager")
	}
	if err := validateOnebotBridgeOptions(bind, token); err != nil {
		return err
	}
	instances := dm.DiceSnapshot()
	if len(instances) != 1 || instances[0] == nil || instances[0].BaseConfig.Name != "default" {
		return errors.New("bridge mode requires exactly one default Dice instance")
	}
	d := instances[0]
	d.onebotBridgeIsolated = true
	d.onebotBridgeBind = bind
	d.onebotBridgeToken = token
	return nil
}

func validateOnebotBridgeOptions(bind, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("ONEBOT_WS_TOKEN is required")
	}
	if strings.TrimSpace(bind) == "" {
		bind = onebotBridgeDefaultBind
	}
	host, portText, err := net.SplitHostPort(bind)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || host == "" || port < 1 || port > 65535 {
		return errors.New("SEALDICE_ONEBOT_BIND must be a host:port address")
	}
	return nil
}

type onebotBridgeServeConfig struct {
	IMSession struct {
		EndPoints []struct {
			Platform     string `yaml:"platform"`
			ProtocolType string `yaml:"protocolType"`
			Adapter      struct {
				LLMBridgeEnabled bool   `yaml:"llmBridgeEnabled"`
				Mode             string `yaml:"mode"`
			} `yaml:"adapter"`
		} `yaml:"endPoints"`
	} `yaml:"imSession"`
	JsEnable                *bool `yaml:"jsEnable"`
	CustomReplyConfigEnable *bool `yaml:"customReplyConfigEnable"`
	ExtDefaultSettings      []struct {
		Name       string `yaml:"name"`
		AutoActive bool   `yaml:"autoActive"`
	} `yaml:"extDefaultSettings"`
}

func validateOnebotBridgeData(dataDir, packageSourceDir, packageCacheDir string) error {
	servePath := filepath.Join(dataDir, "serve.yaml")
	data, err := os.ReadFile(servePath)
	serveExists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.New("bridge data volume cannot be inspected safely")
	}

	if serveExists {
		var config onebotBridgeServeConfig
		if err := yaml.Unmarshal(data, &config); err != nil {
			return errors.New("bridge data volume has an invalid serve.yaml")
		}
		if config.JsEnable == nil || *config.JsEnable {
			return errors.New("bridge data volume must explicitly disable JavaScript extensions")
		}
		if config.CustomReplyConfigEnable == nil || *config.CustomReplyConfigEnable {
			return errors.New("bridge data volume must explicitly disable custom replies")
		}
		for _, ext := range config.ExtDefaultSettings {
			if ext.AutoActive && ext.Name != "coc7" && ext.Name != "dnd5e" && ext.Name != "fun" {
				return errors.New("bridge data volume has an active extension outside the native rule allowlist")
			}
		}
		if len(config.IMSession.EndPoints) > 1 {
			return errors.New("bridge data volume contains unrelated platform endpoints")
		}
		if len(config.IMSession.EndPoints) == 1 {
			endpoint := config.IMSession.EndPoints[0]
			if endpoint.Platform != "QQ" || endpoint.ProtocolType != "pureonebot" || !endpoint.Adapter.LLMBridgeEnabled || endpoint.Adapter.Mode != "server" {
				return errors.New("bridge data volume contains an unrelated platform endpoint")
			}
		}
	} else {
		entries, readErr := os.ReadDir(dataDir)
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return errors.New("bridge data volume cannot be inspected safely")
		}
		if len(entries) != 0 {
			return errors.New("bridge data volume has state but no serve.yaml; refusing to initialize it")
		}
	}

	if err := rejectOnebotBridgeFiles(filepath.Join(dataDir, "scripts"), func(path string) bool {
		ext := strings.ToLower(filepath.Ext(path))
		return ext == ".js" || ext == ".ts" || ext == ".mjs" || ext == ".cjs"
	}); err != nil {
		return errors.New("bridge data volume contains JavaScript scripts")
	}
	if err := rejectOnebotBridgeFiles(filepath.Join(dataDir, "extensions", "reply"), func(path string) bool {
		name := strings.ToLower(filepath.Base(path))
		if name == "info.yaml" || strings.HasPrefix(name, ".reply") {
			return false
		}
		ext := strings.ToLower(filepath.Ext(name))
		return ext == ".yaml" || ext == ".yml" || ext == ""
	}); err != nil {
		return errors.New("bridge data volume contains custom reply files")
	}
	if err := rejectNonEmptyDirectory(packageSourceDir); err != nil {
		return errors.New("bridge data volume contains extension packages")
	}
	if err := rejectNonEmptyDirectory(packageCacheDir); err != nil {
		return errors.New("bridge data volume contains extension packages")
	}
	return nil
}

func rejectOnebotBridgeFiles(root string, reject func(string) bool) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if reject(path) {
			return errors.New("restricted file present")
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func rejectNonEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("directory is not empty")
	}
	return nil
}

func (d *Dice) applyOnebotBridgeIsolation() {
	if d == nil || !d.onebotBridgeIsolated {
		return
	}
	d.Config.JsEnable = false
	d.Config.CustomReplyConfigEnable = false
	d.Config.AliveNoticeEnable = false
	d.Config.Enable = false
	d.CustomReplyConfig = nil
	d.Config.ExtDefaultSettings = []*ExtDefaultSettingItem{
		{Name: "coc7", AutoActive: true, DisabledCommand: map[string]bool{}},
		{Name: "dnd5e", AutoActive: true, DisabledCommand: map[string]bool{}},
		{Name: "fun", AutoActive: true, DisabledCommand: map[string]bool{}},
	}
	d.ApplyExtDefaultSettings()

	var bridgeEndpoint *EndPointInfo
	for _, endpoint := range d.ImSession.EndPoints {
		adapter, ok := endpoint.Adapter.(*PlatformAdapterOnebot)
		if !ok || !adapter.LLMBridgeEnabled {
			continue
		}
		bridgeEndpoint = endpoint
		break
	}
	if bridgeEndpoint == nil {
		bridgeEndpoint = NewOnebotConnItem(AddOnebotEcho{
			Mode:          "server",
			ReverseURL:    d.onebotBridgeBind,
			ReverseSuffix: onebotBridgeSuffix,
		})
		d.ImSession.EndPoints = append(d.ImSession.EndPoints, bridgeEndpoint)
	}
	adapter := bridgeEndpoint.Adapter.(*PlatformAdapterOnebot)
	bridgeEndpoint.Platform = "QQ"
	bridgeEndpoint.ProtocolType = "pureonebot"
	bridgeEndpoint.Enable = true
	adapter.EndPoint = bridgeEndpoint
	adapter.Mode = "server"
	adapter.ReverseUrl = d.onebotBridgeBind
	adapter.ReverseSuffix = onebotBridgeSuffix
	adapter.ConnectURL = ""
	adapter.Token = ""
	adapter.LLMBridgeEnabled = true
	adapter.bridgeRuntimeToken = d.onebotBridgeToken
	adapter.bridgeRuntimeOnly = true
	bridgeEndpoint.BindRuntime(d.ImSession)
	d.ImSession.RefreshEndPointsSnapshot()
}

func onebotBridgeSessionForSave(session *IMSession) *IMSession {
	if session == nil {
		return nil
	}
	filtered := &IMSession{EndPoints: make([]*EndPointInfo, 0, len(session.EndPoints))}
	for _, endpoint := range session.EndPoints {
		if endpoint == nil {
			continue
		}
		adapter, isOnebot := endpoint.Adapter.(*PlatformAdapterOnebot)
		if isOnebot && adapter.bridgeRuntimeOnly {
			continue
		}
		filtered.EndPoints = append(filtered.EndPoints, endpoint)
	}
	return filtered
}
