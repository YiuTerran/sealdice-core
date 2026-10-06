//nolint:testpackage // These tests cover bootstrap state that is intentionally kept private.
package dice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateOnebotBridgeDataAllowsFreshVolume(t *testing.T) {
	root := t.TempDir()
	if err := validateOnebotBridgeData(filepath.Join(root, "default"), filepath.Join(root, "packages"), filepath.Join(root, "cache", "packages")); err != nil {
		t.Fatalf("fresh bridge volume should be accepted: %v", err)
	}
}

func TestOnebotBridgePreflightRunsBeforeDatabaseInitialization(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "default")
	packagesDir := filepath.Join(root, "packages")
	packageCacheDir := filepath.Join(root, "cache", "packages")
	if err := preflightOnebotBridgeVolume("0.0.0.0:18081", "runtime-token", dataDir, packagesDir, packageCacheDir); err != nil {
		t.Fatalf("fresh bridge volume should pass preflight: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("preflight must not initialize the Dice data directory, stat err=%v", err)
	}

	// The normal database open creates this state before TryCreateDefault and
	// Dice.Init. Runtime configuration must not mistake those fresh DB files
	// for pre-existing user data after preflight has already succeeded.
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "data.db"), []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	dm := &DiceManager{Dice: []*Dice{{BaseConfig: BaseConfig{Name: "default"}}}}
	if err := ConfigureOnebotLLMBridge(dm, "0.0.0.0:18081", "runtime-token"); err != nil {
		t.Fatalf("post-preflight runtime configuration must allow newly initialized DB files: %v", err)
	}
	if !dm.Dice[0].onebotBridgeIsolated {
		t.Fatal("runtime bridge isolation was not enabled")
	}
}

func TestValidateOnebotBridgeDataRequiresExplicitSafeExistingConfig(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "default")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	serve := `imSession:
  endPoints: []
jsEnable: false
customReplyConfigEnable: false
extDefaultSettings:
  - name: coc7
    autoActive: true
  - name: dnd5e
    autoActive: true
`
	if err := os.WriteFile(filepath.Join(dataDir, "serve.yaml"), []byte(serve), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOnebotBridgeData(dataDir, filepath.Join(root, "packages"), filepath.Join(root, "cache", "packages")); err != nil {
		t.Fatalf("safe existing bridge volume should be accepted: %v", err)
	}

	unsafe := strings.Replace(serve, "jsEnable: false", "jsEnable: true", 1)
	if err := os.WriteFile(filepath.Join(dataDir, "serve.yaml"), []byte(unsafe), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOnebotBridgeData(dataDir, filepath.Join(root, "packages"), filepath.Join(root, "cache", "packages")); err == nil {
		t.Fatal("expected enabled JavaScript to reject bridge startup")
	}
}

func TestValidateOnebotBridgeDataRejectsUnrelatedFeatures(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "default")
	if err := os.MkdirAll(filepath.Join(dataDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	serve := `imSession:
  endPoints: []
jsEnable: false
customReplyConfigEnable: false
extDefaultSettings:
  - name: third-party
    autoActive: true
`
	if err := os.WriteFile(filepath.Join(dataDir, "serve.yaml"), []byte(serve), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOnebotBridgeData(dataDir, filepath.Join(root, "packages"), filepath.Join(root, "cache", "packages")); err == nil {
		t.Fatal("expected active non-native extension to reject bridge startup")
	}

	serve = strings.Replace(serve, "  - name: third-party\n    autoActive: true\n", "", 1)
	if err := os.WriteFile(filepath.Join(dataDir, "serve.yaml"), []byte(serve), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "scripts", "untrusted.js"), []byte("secret command body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOnebotBridgeData(dataDir, filepath.Join(root, "packages"), filepath.Join(root, "cache", "packages")); err == nil {
		t.Fatal("expected script file to reject bridge startup")
	}
}

func TestOnebotBridgeIsolationAddsRuntimeOnlyEndpointAndSafeConfig(t *testing.T) {
	dice := &Dice{
		onebotBridgeIsolated: true,
		onebotBridgeBind:     "0.0.0.0:18081",
		onebotBridgeToken:    "runtime-secret",
		ImSession:            &IMSession{},
		Config: Config{
			JsConfig: JsConfig{JsEnable: true},
			BaseConfig: BaseConfig{
				CustomReplyConfigEnable: true,
				AliveNoticeEnable:       true,
			},
		},
	}
	dice.applyOnebotBridgeIsolation()
	if dice.Config.JsEnable || dice.Config.CustomReplyConfigEnable || dice.Config.AliveNoticeEnable {
		t.Fatal("bridge isolation did not disable scripts, custom replies, and alive notices")
	}
	if len(dice.ImSession.EndPoints) != 1 {
		t.Fatalf("expected one runtime endpoint, got %d", len(dice.ImSession.EndPoints))
	}
	adapter := dice.ImSession.EndPoints[0].Adapter.(*PlatformAdapterOnebot)
	if !adapter.LLMBridgeEnabled || !adapter.bridgeRuntimeOnly || adapter.Token != "" || adapter.bridgeRuntimeToken != "runtime-secret" {
		t.Fatalf("unexpected runtime adapter config: %#v", adapter)
	}

	encoded, err := yaml.Marshal(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "runtime-secret") || !strings.Contains(string(encoded), "token: \"\"") {
		t.Fatalf("runtime token appeared in serialized adapter: %s", encoded)
	}
	filtered := onebotBridgeSessionForSave(dice.ImSession)
	if len(filtered.EndPoints) != 0 {
		t.Fatal("runtime-only endpoint should be omitted from serve.yaml persistence")
	}
}

func TestOnebotBridgeRuntimeRegistersOnlyReviewedNativeExtensions(t *testing.T) {
	dice := &Dice{
		onebotBridgeIsolated: true,
		onebotBridgeBind:     "0.0.0.0:18081",
		onebotBridgeToken:    "runtime-secret",
		ImSession:            &IMSession{},
		CmdMap:               CmdMapCls{},
		GameSystemMap:        new(SyncMap[string, *GameSystemTemplate]),
	}
	dice.registerBuiltinExtForRuntime()
	if len(dice.ExtList) != 4 || dice.ExtList[0].Name != "coc7" || dice.ExtList[1].Name != "dnd5e" || dice.ExtList[2].Name != "fun" || dice.ExtList[3].Name != "bridge-log" {
		t.Fatalf("bridge extension registry = %#v, want coc7, dnd5e, reviewed fun, and parser-only log", dice.ExtList)
	}
	parserLog := dice.ExtList[3]
	if parserLog.AutoActive || parserLog.GetCmdMap()["log"] == nil || parserLog.GetCmdMap()["log"].Solve != nil ||
		parserLog.OnCommandReceived != nil || parserLog.OnMessageReceived != nil || parserLog.OnMessageSend != nil {
		t.Fatalf("bridge log parser must contain no handlers or hooks: %#v", parserLog)
	}
	dice.applyOnebotBridgeIsolation()
	if len(dice.Config.ExtDefaultSettings) != 4 {
		t.Fatalf("bridge default extension settings = %#v, want three active extensions and inactive bridge-log", dice.Config.ExtDefaultSettings)
	}
	for _, setting := range dice.Config.ExtDefaultSettings {
		if setting.Name == "bridge-log" && setting.AutoActive {
			t.Fatal("bridge-log parser extension must stay inactive in groups")
		}
	}
	fun, _ := dice.ExtRegistry.Load("fun")
	if fun == nil || fun.OnCommandReceived != nil || fun.OnLoad != nil || fun.OnPoke != nil {
		t.Fatalf("bridge fun extension retained event hooks: %#v", fun)
	}
	for _, command := range []string{"ping", "gugu", "咕咕", "jrrp", "rsr", "ek", "dx", "ww"} {
		if _, ok := fun.CmdMap[command]; !ok {
			t.Errorf("reviewed fun command %q was not registered", command)
		}
	}
	for _, command := range []string{"alias", "&", "a", "send", "welcome", "text", "ekgen", "dxh", "wh", "wwh", "rh", "rx"} {
		if _, ok := fun.CmdMap[command]; ok {
			t.Errorf("unreviewed fun command %q was registered", command)
		}
	}

	type savedBridgeConfig struct {
		IMSession               *IMSession               `yaml:"imSession"`
		JsEnable                bool                     `yaml:"jsEnable"`
		CustomReplyConfigEnable bool                     `yaml:"customReplyConfigEnable"`
		ExtDefaultSettings      []*ExtDefaultSettingItem `yaml:"extDefaultSettings"`
	}
	data, err := yaml.Marshal(savedBridgeConfig{
		IMSession:               onebotBridgeSessionForSave(dice.ImSession),
		JsEnable:                dice.Config.JsEnable,
		CustomReplyConfigEnable: dice.Config.CustomReplyConfigEnable,
		ExtDefaultSettings:      dice.Config.ExtDefaultSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "default")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "serve.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOnebotBridgeData(dataDir, filepath.Join(root, "packages"), filepath.Join(root, "cache", "packages")); err != nil {
		t.Fatalf("safe bridge config should pass the next-start preflight: %v\n%s", err, data)
	}
}
