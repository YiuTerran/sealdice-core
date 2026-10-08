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

func TestOnebotBridgeLogDisplayMetadataRendersNamesWithoutVirtualIDs(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logDB := d.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	const groupNumber int64 = 8000000000000121
	const groupID = "QQ-Group:8000000000000121"
	if _, err := service.OnebotBridgeLogNew(d.DBOperator, groupID, "display"); err != nil {
		t.Fatal(err)
	}
	em := &onebotBridgeCaptureEmitter{}
	pa := &PlatformAdapterOnebot{EndPoint: ep, LLMBridgeEnabled: true, logger: zap.NewNop().Sugar()}
	ep.Adapter = pa
	ep.Session = d.ImSession
	kws := &socketio.WebsocketWrapper{}
	conn := &onebotBridgeConnection{emitter: em, ctx: context.Background(), registerDone: make(chan struct{})}
	conn.setRegistration("display-connection", true)
	conn.setLogDisplayCapability("display-connection", true)
	pa.bridgeConnections = map[*socketio.WebsocketWrapper]*onebotBridgeConnection{kws: conn}
	conn.startDispatch(func(raw []byte) { pa.processOnebotMessageEvent(raw, conn) })
	defer conn.close()

	type frame struct {
		PostType     string                  `json:"post_type"`
		Version      int                     `json:"version"`
		ConnectionID string                  `json:"connection_id"`
		EventID      string                  `json:"event_id"`
		GroupID      int64                   `json:"group_id"`
		UserID       int64                   `json:"user_id"`
		Time         int64                   `json:"time"`
		Nickname     string                  `json:"nickname"`
		Text         string                  `json:"text"`
		IsBot        bool                    `json:"is_bot"`
		Kind         string                  `json:"kind"`
		Display      *onebotBridgeLogDisplay `json:"display,omitempty"`
	}
	frames := []frame{
		{
			PostType: onebotLLMBridgeLogEventPostType, Version: 1, ConnectionID: "display-connection", EventID: "display-event-1",
			GroupID: groupNumber, UserID: 8000000000000122, Time: 1710000101, Nickname: "Author & One",
			Text: "<@friend> [@789](mqqapi://markdown/mention?at_type=1&at_tinyid=789) <@unknown> <@!unknown> [@999](mqqapi://markdown/mention?at_type=1&at_tinyid=999) <@shared> <@no-name> <@bot> [@Inline](mqqapi://markdown/mention?at_type=1&at_tinyid=456) [@Old Label](mqqapi://markdown/mention?at_tinyid=123&at_type=1) [@Literal](mqqapi://markdown/mention?at_type=1&at_tinyid=654) [@openid:shared](mqqapi://markdown/mention?at_type=1&at_tinyid=655)",
			Kind: "message", Display: &onebotBridgeLogDisplay{
				AuthorAliases: []string{"openid:source"},
				Mentions: []onebotBridgeLogMention{
					{Target: "tinyid:123", Aliases: []string{"openid:linked"}, Name: "SDK <Current>"},
					{Target: "tinyid:789", Aliases: []string{"openid:friend"}, Name: "openid:friend"},
					{Target: "tinyid:999", Aliases: []string{"openid:unknown"}, Name: "999"},
					{Target: "tinyid:654", Aliases: []string{"openid:shared"}, Name: "SDK conflict", IsBot: true},
					{Target: "tinyid:655", Aliases: []string{"openid:shared"}, Name: "openid:shared"},
					{Target: "tinyid:321", Aliases: []string{"openid:no-name"}},
					{Target: "openid:bot", IsBot: true, Name: "ignored bot nickname"},
				},
			},
		},
		{
			PostType: onebotLLMBridgeLogEventPostType, Version: 1, ConnectionID: "display-connection", EventID: "display-event-2",
			GroupID: groupNumber, UserID: 8000000000000123, Time: 1710000102, Nickname: "Friend <Name>", Text: "friend's message", Kind: "message",
			Display: &onebotBridgeLogDisplay{AuthorAliases: []string{"openid:friend"}},
		},
		{
			PostType: onebotLLMBridgeLogEventPostType, Version: 1, ConnectionID: "display-connection", EventID: "display-event-3",
			GroupID: groupNumber, UserID: 8000000000000124, Time: 1710000103, Nickname: "Other", Text: "other's message", Kind: "message",
			Display: &onebotBridgeLogDisplay{AuthorAliases: []string{"openid:shared"}},
		},
		{
			PostType: onebotLLMBridgeLogEventPostType, Version: 1, ConnectionID: "display-connection", EventID: "display-event-4",
			GroupID: groupNumber, UserID: 8000000000000125, Time: 1710000104, Nickname: "Other Two", Text: "conflicting alias", Kind: "message",
			Display: &onebotBridgeLogDisplay{AuthorAliases: []string{"openid:shared"}},
		},
		{
			PostType: onebotLLMBridgeLogEventPostType, Version: 1, ConnectionID: "display-connection", EventID: "display-event-5",
			GroupID: groupNumber, UserID: 8000000000000126, Time: 1710000105, Nickname: "", Text: "speaker name missing", Kind: "message",
			Display: &onebotBridgeLogDisplay{AuthorAliases: []string{"openid:no-name"}},
		},
	}
	for index, event := range frames {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		dispatchOnebotRaw(t, pa, kws, string(encoded))
		waitOnebotBridgeAction(t, em, index+1, onebotLLMBridgeLogAckAction)
	}
	duplicate := frames[0]
	duplicate.Display = &onebotBridgeLogDisplay{AuthorAliases: []string{"openid:source"}, Mentions: []onebotBridgeLogMention{{Target: "tinyid:123", Name: "Changed after acceptance"}}}
	duplicateRaw, err := json.Marshal(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	dispatchOnebotRaw(t, pa, kws, string(duplicateRaw))
	waitOnebotBridgeAction(t, em, len(frames)+1, onebotLLMBridgeLogAckAction)
	markdown, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "display", "md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(markdown)
	for _, internal := range []string{"8000000000000121", "8000000000000122", "virtual:", "<code>"} {
		if strings.Contains(text, internal) {
			t.Fatalf("Markdown leaked internal identity %q: %s", internal, text)
		}
	}
	for _, want := range []string{"@SDK &lt;Current&gt;", "@Inline", "@Friend &lt;Name&gt;", "@机器人", "@成员1", "@成员2", "@Literal", "成员3", "Author &amp; One"} {
		if !strings.Contains(text, want) {
			t.Errorf("Markdown missing %q: %s", want, text)
		}
	}
	if strings.Count(text, "@成员1") != 3 {
		t.Fatalf("repeated unknown target did not keep a stable anonymous label: %s", text)
	}
	plain, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "display", "txt")
	if err != nil {
		t.Fatal(err)
	}
	plainText := string(plain)
	for _, internal := range []string{"8000000000000121", "8000000000000122", "virtual:"} {
		if strings.Contains(plainText, internal) {
			t.Fatalf("TXT leaked internal identity %q: %s", internal, plainText)
		}
	}
	if !strings.Contains(plainText, frames[0].Text) {
		t.Fatalf("TXT body did not retain the stored original mention syntax: %s", plainText)
	}
	if got := onebotBridgeArtifactFilename("display log", "md"); got != "display-log.md" {
		t.Fatalf("artifact filename contains unexpected identity or normalization: %q", got)
	}
}

func TestOnebotBridgeLogDisplayAcceptsEightLinkedAliases(t *testing.T) {
	display := onebotBridgeLogDisplay{
		AuthorAliases: []string{"openid:author"},
		Mentions: []onebotBridgeLogMention{{
			Target: "openid:primary",
			Aliases: []string{
				"openid:primary", "openid:alias-2", "openid:alias-3", "openid:alias-4",
				"tinyid:1", "tinyid:2", "tinyid:3", "tinyid:4",
			},
		}},
	}
	encoded, err := json.Marshal(display)
	if err != nil {
		t.Fatal(err)
	}
	if parsed := parseOnebotBridgeLogDisplay(encoded); parsed == nil || len(parsed.Mentions[0].Aliases) != 7 {
		t.Fatalf("eight aliases including a redundant target should be accepted and deduplicated: %#v", parsed)
	}
	display.Mentions[0].Aliases = []string{
		"openid:alias-2", "openid:alias-3", "openid:alias-4", "openid:alias-5",
		"tinyid:1", "tinyid:2", "tinyid:3", "tinyid:4",
	}
	encoded, err = json.Marshal(display)
	if err != nil {
		t.Fatal(err)
	}
	if parsed := parseOnebotBridgeLogDisplay(encoded); parsed != nil {
		t.Fatalf("nine linked identities including target should be rejected: %#v", parsed)
	}
}

func TestOnebotBridgeLogDisplayFindAliasCompressesDeepChainsIteratively(t *testing.T) {
	writer := newOnebotBridgeLogSnapshotWriter(&strings.Builder{}, "md")
	const chainLength = 20_000
	for index := chainLength; index > 0; index-- {
		writer.aliasParent[fmt.Sprintf("alias:%05d", index)] = fmt.Sprintf("alias:%05d", index-1)
	}
	if got := writer.findAlias(fmt.Sprintf("alias:%05d", chainLength)); got != "alias:00000" {
		t.Fatalf("deep alias root = %q, want alias:00000", got)
	}
}

func TestOnebotBridgeInvalidDisplayMetadataDoesNotRejectCapture(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logDB := d.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	const groupID = "QQ-Group:8000000000000131"
	if _, err := service.OnebotBridgeLogNew(d.DBOperator, groupID, "oversize-display"); err != nil {
		t.Fatal(err)
	}
	em := &onebotBridgeCaptureEmitter{}
	pa := &PlatformAdapterOnebot{EndPoint: ep, LLMBridgeEnabled: true, logger: zap.NewNop().Sugar()}
	ep.Adapter = pa
	ep.Session = d.ImSession
	kws := &socketio.WebsocketWrapper{}
	conn := &onebotBridgeConnection{emitter: em, ctx: context.Background(), registerDone: make(chan struct{})}
	conn.setRegistration("display-connection", true)
	conn.setLogDisplayCapability("display-connection", true)
	pa.bridgeConnections = map[*socketio.WebsocketWrapper]*onebotBridgeConnection{kws: conn}
	conn.startDispatch(func(raw []byte) { pa.processOnebotMessageEvent(raw, conn) })
	defer conn.close()

	raw := `{"post_type":"_llm_bridge_log_event","version":1,"connection_id":"display-connection","event_id":"oversize-display-event","group_id":8000000000000131,"user_id":8000000000000132,"time":1710000201,"nickname":"Member","text":"body stays","is_bot":false,"kind":"message","display":{"author_aliases":["openid:member"],"mentions":[{"target":"tinyid:123","name":"` + strings.Repeat("x", 257) + `"}]}}`
	dispatchOnebotRaw(t, pa, kws, raw)
	waitOnebotBridgeAction(t, em, 1, onebotLLMBridgeLogAckAction)
	tooLargeDisplay := json.RawMessage(`{"author_aliases":["openid:member"],"padding":"` + strings.Repeat("x", onebotBridgeLogDisplayMaxBytes) + `"}`)
	isBot := false
	oversizeEvent := onebotBridgeLogEvent{
		PostType: onebotLLMBridgeLogEventPostType, Version: 1, ConnectionID: "display-connection", EventID: "oversize-display-event-2",
		GroupID: 8000000000000131, UserID: 8000000000000133, Time: 1710000202, Nickname: "Member", Text: "second body",
		IsBot: &isBot, Kind: "message", Display: tooLargeDisplay,
	}
	oversizeRaw, err := json.Marshal(oversizeEvent)
	if err != nil {
		t.Fatal(err)
	}
	dispatchOnebotRaw(t, pa, kws, string(oversizeRaw))
	waitOnebotBridgeAction(t, em, 2, onebotLLMBridgeLogAckAction)
	items, err := service.LogGetAllLines(d.DBOperator, groupID, "oversize-display")
	if err != nil || len(items) != 2 || items[0].Message != "body stays" || items[1].Message != "second body" {
		t.Fatalf("invalid display metadata rejected source capture: items=%#v err=%v", items, err)
	}
	for _, item := range items {
		info, ok := item.CommandInfo.(map[string]interface{})
		if !ok {
			t.Fatalf("command info was not decoded: %#v", item.CommandInfo)
		}
		if _, exists := info["bridgeDisplay"]; exists {
			t.Fatalf("invalid display metadata was persisted: %#v", info)
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
