//nolint:testpackage // These tests exercise private bridge capture and emitter state.
package dice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	socketio "github.com/PaienNate/pineutil/evsocket/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	emitter "sealdice-core/dice/imsdk/onebot"
	"sealdice-core/dice/service"
	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

func TestOnebotBridgeCaptureFrameIngressACKDedupAndSafeRendering(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logDB := d.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	const groupNumber int64 = 8000000000000001
	const userNumber int64 = 8000000000000002
	groupID := fmt.Sprintf("QQ-Group:%d", groupNumber)
	if _, err := service.OnebotBridgeLogNew(d.DBOperator, groupID, "capture"); err != nil {
		t.Fatal(err)
	}
	logCore, observed := observer.New(zapcore.DebugLevel)
	d.Logger = zap.New(logCore).Sugar()
	em := &onebotBridgeCaptureEmitter{}
	pa := &PlatformAdapterOnebot{EndPoint: ep, LLMBridgeEnabled: true, logger: d.Logger}
	ep.Adapter = pa
	ep.Session = d.ImSession
	kws := &socketio.WebsocketWrapper{}
	conn := &onebotBridgeConnection{emitter: em, ctx: context.Background(), registerDone: make(chan struct{})}
	conn.setRegistration("capture-connection", true)
	pa.bridgeConnections = map[*socketio.WebsocketWrapper]*onebotBridgeConnection{kws: conn}
	conn.startDispatch(func(raw []byte) { pa.processOnebotMessageEvent(raw, conn) })
	defer conn.close()

	const raw = `{"post_type":"_llm_bridge_log_event","version":1,"connection_id":"capture-connection","event_id":"capture-event-0001","group_id":8000000000000001,"user_id":8000000000000002,"time":1710000001,"nickname":"A <b>lice","text":"hello <script>secret</script>","is_bot":false,"kind":"message"}`
	dispatchOnebotRaw(t, pa, kws, raw)
	firstAck := waitOnebotBridgeAction(t, em, 1, onebotLLMBridgeLogAckAction)
	var ack onebotBridgeLogAckParams
	if err := json.Unmarshal(firstAck.params, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.ConnectionID != "capture-connection" || ack.EventID != "capture-event-0001" || ack.Status != "ok" {
		t.Fatalf("unexpected durable capture ACK: %#v", ack)
	}
	// Replay the exact numeric JSON frame. It receives another ACK but one native row.
	dispatchOnebotRaw(t, pa, kws, raw)
	waitOnebotBridgeAction(t, em, 2, onebotLLMBridgeLogAckAction)

	wrongConnection := strings.Replace(raw, "capture-connection", "stale-connection", 1)
	dispatchOnebotRaw(t, pa, kws, wrongConnection)
	stringID := strings.Replace(raw, `"group_id":8000000000000001`, `"group_id":"8000000000000001"`, 1)
	dispatchOnebotRaw(t, pa, kws, stringID)
	badTime := strings.Replace(raw, `"event_id":"capture-event-0001"`, `"event_id":"capture-event-invalid-time"`, 1)
	badTime = strings.Replace(badTime, `"time":1710000001`, `"time":0`, 1)
	dispatchOnebotRaw(t, pa, kws, badTime)
	thirdAck := waitOnebotBridgeAction(t, em, 3, onebotLLMBridgeLogAckAction)
	if err := json.Unmarshal(thirdAck.params, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.EventID != "capture-event-invalid-time" || ack.Status != "failed" {
		t.Fatalf("invalid timestamp must receive a failed ACK: %#v", ack)
	}
	if got := capturedOnebotBridgeActionCount(em); got != 3 {
		t.Fatalf("stale connection or string virtual ID produced an ACK; actions=%d", got)
	}
	em.rawActionErr = errors.New("secret-log-ack-action-error")
	em.rawActionName = onebotLLMBridgeLogAckAction
	offGroupFrame := strings.Replace(raw, `"event_id":"capture-event-0001"`, `"event_id":"capture-event-ack-error"`, 1)
	offGroupFrame = strings.Replace(offGroupFrame, `"group_id":8000000000000001`, `"group_id":8000000000000090`, 1)
	offGroupFrame = strings.Replace(offGroupFrame, `"text":"hello <script>secret</script>"`, `"text":"secret-ack-body"`, 1)
	dispatchOnebotRaw(t, pa, kws, offGroupFrame)
	waitOnebotBridgeAction(t, em, 4, onebotLLMBridgeLogAckAction)
	items, err := service.LogGetAllLines(d.DBOperator, groupID, "capture")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Time != 1710000001 || items[0].IMUserID != strconv.FormatInt(userNumber, 10) ||
		items[0].UniformID != fmt.Sprintf("OneBotBridge:QQ:%d", userNumber) || items[0].Message != "hello <script>secret</script>" {
		t.Fatalf("capture item mismatch: %#v", items)
	}
	markdown, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "capture", "md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markdown), `style="color:#`) || !strings.Contains(string(markdown), "&lt;script&gt;secret&lt;/script&gt;") || strings.Contains(string(markdown), "<script>") {
		t.Fatalf("Markdown renderer failed stable color or escaped body: %s", markdown)
	}
	pre := regexp.MustCompile(`(?s)<pre style="[^"]+">(.*?)</pre>`).FindStringSubmatch(string(markdown))
	if len(pre) != 2 || html.UnescapeString(pre[1]) != "hello <script>secret</script>" {
		t.Fatalf("Markdown pre content did not round-trip raw text: %#v", pre)
	}
	plainText, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "capture", "txt")
	if err != nil || !strings.Contains(string(plainText), "hello <script>secret</script>") {
		t.Fatalf("TXT renderer changed raw message: err=%v output=%s", err, plainText)
	}
	for _, entry := range observed.All() {
		if strings.Contains(entry.Message, "secret") || strings.Contains(fmt.Sprint(entry.ContextMap()), "secret") {
			t.Fatalf("capture diagnostics leaked body text: %#v", entry)
		}
	}
}

func TestOnebotBridgeSnapshotRejectsOversizeWithoutArtifact(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logDB := d.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	const groupID = "QQ-Group:8000000000000031"
	logID, err := service.LogGetOrCreate(d.DBOperator, groupID, "too-large")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 1024*1024)
	items := make([]model.LogOneItem, 11)
	for index := range items {
		items[index] = model.LogOneItem{LogID: logID, GroupID: groupID, Nickname: "member", IMUserID: "8000000000000032", UniformID: "OneBotBridge:QQ:8000000000000032", Time: int64(1710000000 + index), Message: body}
	}
	if createErr := logDB.Create(&items).Error; createErr != nil {
		t.Fatal(createErr)
	}
	data, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "too-large", "txt")
	if !errors.Is(err, errOnebotBridgeArtifactTooLarge) || data != nil {
		t.Fatalf("oversize export must fail atomically: bytes=%d err=%v", len(data), err)
	}
}

func TestOnebotBridgeArtifactACKCountsOnlySuccessfulSafeAction(t *testing.T) {
	pa := &PlatformAdapterOnebot{LLMBridgeEnabled: true, logger: zap.NewNop().Sugar()}
	em := &onebotBridgeCaptureEmitter{}
	conn, tracker := newOnebotBridgeTestTracker(t, pa, em, "group", 777, onebotBridgeMasterIDMin+1, onebotBridgeMasterIDMin+2, "artifact-connection", "member")
	defer conn.close()
	for _, filename := range []string{"../group-log.md", "group-log\n.md", "group-log\xff.md", strings.Repeat("x", 256) + ".md"} {
		if err := tracker.emitArtifact(filename, "text/markdown", []byte("body")); err == nil {
			t.Fatalf("unsafe filename %q accepted", filename)
		}
	}
	if err := tracker.emitArtifact("group-log.md", "text/plain", []byte("body")); err == nil {
		t.Fatal("mismatched artifact extension and MIME accepted")
	}
	secret := "private-log-body-must-not-appear-in-error"
	if err := tracker.emitArtifact("group-log.md", "text/markdown", []byte(secret)); err != nil {
		t.Fatal(err)
	}
	tracker.mu.Lock()
	count := tracker.outputCount
	tracker.mu.Unlock()
	if count != 1 {
		t.Fatalf("successful artifact ACK output count=%d, want 1", count)
	}
	first := waitOnebotBridgeAction(t, em, 1, onebotLLMBridgeArtifactAction)
	var artifact onebotBridgeArtifactParams
	if err := json.Unmarshal(first.params, &artifact); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(artifact.BytesBase64)
	if err != nil || string(decoded) != secret || artifact.MediaType != "text/markdown" {
		t.Fatalf("artifact wire params mismatch: %#v err=%v", artifact, err)
	}

	em.rawActionErr = errors.New("secret-action-error")
	em.rawActionName = onebotLLMBridgeArtifactAction
	_, failedTracker := newOnebotBridgeTestTracker(t, pa, em, "group", 778, onebotBridgeMasterIDMin+1, onebotBridgeMasterIDMin+2, "artifact-connection-2", "member")
	err = failedTracker.emitArtifact("group-log.md", "text/markdown", []byte(secret))
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(secret))) {
		t.Fatalf("raw artifact action error leaked content: %v", err)
	}
	failedTracker.mu.Lock()
	count = failedTracker.outputCount
	failedTracker.mu.Unlock()
	if count != 0 {
		t.Fatalf("failed artifact ACK output count=%d, want 0", count)
	}
}

func dispatchOnebotRaw(t *testing.T, pa *PlatformAdapterOnebot, kws *socketio.WebsocketWrapper, raw string) {
	t.Helper()
	pa.serveOnebotEvent(&socketio.EventPayload{Kws: kws, Data: []byte(raw)})
}

func waitOnebotBridgeAction(t *testing.T, em *onebotBridgeCaptureEmitter, count int, action emitter.Action) onebotBridgeCapturedAction {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		em.mu.Lock()
		if len(em.actions) >= count {
			got := em.actions[count-1]
			em.mu.Unlock()
			if got.action != action {
				t.Fatalf("action[%d]=%s, want %s", count-1, got.action, action)
			}
			return got
		}
		em.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for action %s count %d", action, count)
	return onebotBridgeCapturedAction{}
}

func capturedOnebotBridgeActionCount(em *onebotBridgeCaptureEmitter) int {
	em.mu.Lock()
	defer em.mu.Unlock()
	return len(em.actions)
}
