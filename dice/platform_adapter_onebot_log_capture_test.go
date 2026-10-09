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
	bodies := onebotBridgeMarkdownRenderedBodies(t, markdown)
	if len(bodies) != 1 || bodies[0] != "hello <script>secret</script>" {
		t.Fatalf("Markdown body did not round-trip raw text: %#v", bodies)
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

func TestOnebotBridgeMarkdownBodyUsesSingleLineSpansAndPreservesText(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logDB := d.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	const groupID = "QQ-Group:8000000000000119"
	if _, err := service.OnebotBridgeLogNew(d.DBOperator, groupID, "multiline"); err != nil {
		t.Fatal(err)
	}
	message := strings.Join([]string{
		"first line",
		"",
		"- list item",
		"|left|right|",
		"|---|---|",
		"`code` with\ttab",
		"<script>alert('x')</script>",
		"last line\r",
		"bare\rseparator",
		"",
	}, "\n")
	item := &model.LogOneItem{
		Nickname: "Member", IMUserID: "8000000000000120", UniformID: "OneBotBridge:QQ:8000000000000120",
		Time: 1710000301, Message: message, CommandInfo: map[string]interface{}{"bridgeCaptureKind": "message"},
	}
	if _, _, err := service.LogCaptureAppend(d.DBOperator, "multiline-event-1", groupID, "message", item); err != nil {
		t.Fatal(err)
	}
	markdown, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "multiline", "md")
	if err != nil {
		t.Fatal(err)
	}
	div := regexp.MustCompile(`<div>(.*?)</div>`).FindStringSubmatch(string(markdown))
	if len(div) != 2 || strings.Contains(div[1], "<script>") || !strings.Contains(div[1], "&lt;script&gt;") {
		t.Fatalf("Markdown body was not a single escaped HTML line: %#v", div)
	}
	if strings.ContainsAny(div[1], "\r\n") {
		t.Fatalf("Markdown body contains a physical source newline: %q", div[1])
	}
	spanCount := strings.Count(div[1], `<span style="white-space:pre-wrap;color:`)
	wantMarkdownBody := strings.ReplaceAll(strings.ReplaceAll(message, "\r\n", "\n"), "\r", "\n")
	wantSpanCount := strings.Count(wantMarkdownBody, "\n") + 1
	if spanCount != wantSpanCount {
		t.Fatalf("body span count = %d, want one span for each of %d displayed lines: %s", spanCount, wantSpanCount, div[1])
	}
	if strings.Count(div[1], "<br />") != spanCount-1 {
		t.Fatalf("body has an unexpected number of line breaks: %s", div[1])
	}
	bodies := onebotBridgeMarkdownRenderedBodies(t, markdown)
	if len(bodies) != 1 || bodies[0] != wantMarkdownBody {
		t.Fatalf("Markdown body did not preserve displayed lines, blanks, tabs, or text: got=%q want=%q", bodies, wantMarkdownBody)
	}
	plain, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "multiline", "txt")
	if err != nil || !strings.Contains(string(plain), message+"\n\n") {
		t.Fatalf("TXT renderer changed raw multiline body: err=%v output=%q", err, plain)
	}
}

func TestOnebotBridgeBotMarkdownRendersSafeRichHTMLAndKeepsLineBreaks(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logDB := d.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	const groupID = "QQ-Group:8000000000000135"
	if _, err := service.OnebotBridgeLogNew(d.DBOperator, groupID, "bot-markdown"); err != nil {
		t.Fatal(err)
	}
	firstBody := "**strong** and *italic*\n\n- one `inline`\n- [CQ:at,qq=cq-one]\n\n| left | right |\n| --- | --- |\n| [CQ:at,qq=cq-two] | b |\n\n```go\nline one\nline two\n```\n\n[@sdk](mqqapi://markdown/mention?at_type=1&at_tinyid=901) <@native> code `<@native>`\n\n<script>alert('x')</script>\n\n![remote alt](https://example.test/remote.png) [safe link](https://example.test/page) [unsafe](javascript:alert(1)) [@bad](mqqapi://markdown/mention?at_type=2&at_tinyid=999) [@dup](mqqapi://markdown/mention?at_type=1&at_type=1&at_tinyid=998)"
	unclosedFenceBody := "plain line one\nplain line two\n\n```go\nunclosed code"
	rawHTMLBody := "<section onclick=\"run()\">\n<script>\nattack()\n</script>\n</section>"
	plainBody := "first\nsecond\nthird"
	inlineHTMLBody := "before<span\n title=x data-user='<@z>'>after"
	bodies := []string{firstBody, unclosedFenceBody, rawHTMLBody, plainBody, inlineHTMLBody}
	for index, body := range bodies {
		commandInfo := map[string]interface{}{"bridgeCaptureKind": "message"}
		if index == 0 {
			commandInfo["bridgeDisplay"] = &onebotBridgeLogDisplay{Mentions: []onebotBridgeLogMention{
				{Target: "tinyid:901", Name: "SDK **literal**"},
				{Target: "openid:native", Name: "Native *literal*"},
				{Target: "openid:cq-one", Name: "CQ **one**"},
				{Target: "openid:cq-two", Name: "CQ *two*"},
			}}
		}
		item := &model.LogOneItem{
			Nickname: "机器人", IMUserID: fmt.Sprintf("800000000000013%d", index+6),
			UniformID: fmt.Sprintf("OneBotBridge:QQ:800000000000013%d", index+6),
			Time:      1710000370 + int64(index), Message: body, IsDice: true, CommandInfo: commandInfo,
		}
		if _, _, err := service.LogCaptureAppend(d.DBOperator, fmt.Sprintf("bot-markdown-%d", index), groupID, "bot-markdown", item); err != nil {
			t.Fatalf("append bot message %d: %v", index, err)
		}
	}

	markdown, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "bot-markdown", "md")
	if err != nil {
		t.Fatal(err)
	}
	output := string(markdown)
	divs := regexp.MustCompile(`<div>(.*?)</div>`).FindAllStringSubmatch(output, -1)
	if len(divs) != len(bodies) {
		t.Fatalf("rendered %d bot body containers, want %d: %s", len(divs), len(bodies), output)
	}
	for index, div := range divs {
		if strings.ContainsAny(div[1], "\r\n") {
			t.Fatalf("bot body %d contains a physical source newline: %q", index, div[1])
		}
		if strings.Contains(div[1], "color:") {
			t.Fatalf("bot body %d unexpectedly has inline color: %q", index, div[1])
		}
	}
	rich := divs[0][1]
	for _, expected := range []string{
		"<strong>strong</strong>", "<em>italic</em>", "<ul>", "<code>inline</code>", "<table>",
		"<pre><code class=\"language-go\">line one&#10;line two", "@SDK **literal**", "@Native *literal*",
		"@CQ **one**", "@CQ *two*", "&lt;@native&gt;",
		"&lt;script&gt;alert(", "&lt;/script&gt;", "[image: remote alt]", `href="https://example.test/page"`,
	} {
		if !strings.Contains(rich, expected) {
			t.Errorf("rich bot Markdown body lacks %q: %s", expected, rich)
		}
	}
	if strings.Contains(rich, "https://example.test/remote.png") || strings.Contains(rich, `href="javascript:`) || strings.Contains(rich, "<script>") {
		t.Fatalf("image URL, unsafe link, or executable HTML reached the output: %s", rich)
	}
	if strings.Count(rich, "@Native *literal*") != 1 {
		t.Fatalf("mention inside code span was rewritten or plain mention was missed: %s", rich)
	}
	if strings.Contains(rich, "@bad") || strings.Contains(rich, "@dup") || !strings.Contains(rich, "@成员") {
		t.Fatalf("malformed mention targets used untrusted labels instead of anonymous names: %s", rich)
	}
	if strings.Count(divs[3][1], "<br>") != 2 || !strings.Contains(divs[3][1], "first<br>") || !strings.Contains(divs[3][1], "second<br>") {
		t.Fatalf("plain bot newlines were not kept as visible line breaks: %s", divs[3][1])
	}
	if !strings.Contains(divs[4][1], `<span style="white-space:pre-wrap">&lt;span&#10;title=x data-user=&#39;&lt;@z&gt;&#39;&gt;</span>after`) ||
		strings.Contains(divs[4][1], "<span\n") || strings.Contains(divs[4][1], "@成员") {
		t.Fatalf("multiline inline RawHTML was not escaped with visible line breaks: %s", divs[4][1])
	}
	secondBody := "<div>" + divs[1][1] + "</div>"
	secondBodyStart := strings.Index(output, secondBody)
	secondBodyEnd := secondBodyStart + len(secondBody)
	if !strings.Contains(divs[1][1], "<pre><code class=\"language-go\">unclosed code&#10;</code></pre>") ||
		secondBodyStart < 0 || !strings.Contains(output[secondBodyEnd:], "### <span style=\"color:") {
		t.Fatalf("unclosed fence did not end inside its own bot body: %s", output)
	}
	if !strings.Contains(divs[2][1], "<pre>&lt;section onclick=&#34;run()&#34;&gt;&#10;") ||
		!strings.Contains(divs[2][1], "&lt;script&gt;") || !strings.Contains(divs[2][1], "&#10;") || strings.Contains(divs[2][1], "<script>") {
		t.Fatalf("raw HTML block was omitted or left executable: %s", divs[2][1])
	}

	plain, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "bot-markdown", "txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies {
		if !strings.Contains(string(plain), body+"\n\n") {
			t.Fatalf("TXT renderer changed bot message bytes for %q: %q", body, plain)
		}
	}
}

func TestOnebotBridgeMarkdownMarkupExpansionIsBounded(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logDB := d.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	const groupID = "QQ-Group:8000000000000129"
	if _, err := service.OnebotBridgeLogNew(d.DBOperator, groupID, "markup-limit"); err != nil {
		t.Fatal(err)
	}
	message := strings.Repeat("\n", 200_000)
	item := &model.LogOneItem{
		Nickname: "Member", IMUserID: "8000000000000130", UniformID: "OneBotBridge:QQ:8000000000000130",
		Time: 1710000351, Message: message, CommandInfo: map[string]interface{}{"bridgeCaptureKind": "message"},
	}
	if _, _, err := service.LogCaptureAppend(d.DBOperator, "markup-limit-event", groupID, "message", item); err != nil {
		t.Fatal(err)
	}
	markdown, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "markup-limit", "md")
	if !errors.Is(err, errOnebotBridgeArtifactTooLarge) || markdown != nil {
		t.Fatalf("oversized markup was not rejected without a partial artifact: len=%d err=%v", len(markdown), err)
	}
	botWriter := newOnebotBridgeLogSnapshotWriter(nil, "md")
	if _, err := botWriter.renderBotMarkdownBody(strings.Repeat("&", 2_100_000), nil); !errors.Is(err, errOnebotBridgeArtifactTooLarge) {
		t.Fatalf("expanded bot Markdown exceeded the output limit without an error: %v", err)
	}
}

func TestOnebotBridgeBotMarkdownMentionExpansionBudgetIsCumulative(t *testing.T) {
	const name = "名名"
	w := newOnebotBridgeLogSnapshotWriter(nil, "md")
	display := &onebotBridgeLogDisplay{Mentions: []onebotBridgeLogMention{{Target: "tinyid:budget", Name: name}}}
	budget := len(name)*2 + 1
	token := onebotBridgeLogMentionToken{aliases: []string{"tinyid:budget"}}
	if _, err := w.resolveBotMarkdownMention(token, display, &budget); err != nil {
		t.Fatalf("first mention should fit in the body budget: %v", err)
	}
	if budget != len(name) {
		t.Fatalf("remaining budget = %d, want %d after @name literal", budget, len(name))
	}
	if _, err := w.resolveBotMarkdownMention(token, display, &budget); !errors.Is(err, errOnebotBridgeArtifactTooLarge) {
		t.Fatalf("second repeated mention exceeded budget without rejection: %v", err)
	}
}

func TestOnebotBridgeSnapshotUsesLaterBotAliasesOnlyWhenUncontested(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logDB := d.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	const groupID = "QQ-Group:8000000000000141"
	if _, err := service.OnebotBridgeLogNew(d.DBOperator, groupID, "legacy"); err != nil {
		t.Fatal(err)
	}
	appendItem := func(logName, eventID string, userID int64, nickname, message string, isBot bool, display *onebotBridgeLogDisplay) {
		t.Helper()
		commandInfo := map[string]interface{}{"bridgeCaptureKind": "message"}
		if display != nil {
			commandInfo["bridgeDisplay"] = display
		}
		item := &model.LogOneItem{
			Nickname: nickname, IMUserID: strconv.FormatInt(userID, 10), UniformID: fmt.Sprintf("OneBotBridge:QQ:%d", userID),
			Time: 1710000400 + userID%100, Message: message, IsDice: isBot, CommandInfo: commandInfo,
		}
		if _, _, err := service.LogCaptureAppend(d.DBOperator, eventID, groupID, "message", item); err != nil {
			t.Fatalf("append %s row %q: %v", logName, eventID, err)
		}
	}
	oldBody := "historical [@Ignored Label](mqqapi://markdown/mention?at_type=1&at_tinyid=123) <@legacy-openid> unrelated <@unrelated> nickname <@nickname-only> contested <@claimed> shared <@multi> cross <@cross-bot>"
	appendItem("legacy", "legacy-old", 8000000142, "Legacy speaker", oldBody, false, nil)
	appendItem("legacy", "legacy-nickname", 8000000143, "机器人", "nickname only", false, nil)
	appendItem("legacy", "legacy-bot-metadata", 8000000144, "Recorder", "later SDK evidence",
		false, &onebotBridgeLogDisplay{
			AuthorAliases: []string{"openid:recorder"},
			Mentions: []onebotBridgeLogMention{
				{Target: "tinyid:123", Aliases: []string{"openid:legacy-openid"}, IsBot: true},
				{Target: "tinyid:456", Aliases: []string{"openid:unrelated"}, Name: "Unrelated SDK name"},
			},
		})
	appendItem("legacy", "legacy-human-claim", 8000000145, "Human claimant", "current <@claimed>", false,
		&onebotBridgeLogDisplay{
			AuthorAliases: []string{"openid:claimed"},
			Mentions:      []onebotBridgeLogMention{{Target: "tinyid:901", Aliases: []string{"openid:claimed"}, IsBot: true}},
		})
	appendItem("legacy", "legacy-first-multi-claim", 8000000146, "First claimant", "first claim", false,
		&onebotBridgeLogDisplay{AuthorAliases: []string{"openid:multi"}})
	appendItem("legacy", "legacy-second-multi-claim", 8000000147, "Second claimant", "second claim", false,
		&onebotBridgeLogDisplay{AuthorAliases: []string{"openid:multi"}})
	appendItem("legacy", "legacy-multi-bot-evidence", 8000000148, "Recorder two", "conflicting bot evidence", false,
		&onebotBridgeLogDisplay{
			AuthorAliases: []string{"openid:recorder-two"},
			Mentions:      []onebotBridgeLogMention{{Target: "tinyid:902", Aliases: []string{"openid:multi"}, IsBot: true}},
		})
	if _, err := service.OnebotBridgeLogNew(d.DBOperator, groupID, "other"); err != nil {
		t.Fatal(err)
	}
	appendItem("other", "other-bot-metadata", 8000000149, "Other recorder", "other log evidence", false,
		&onebotBridgeLogDisplay{
			AuthorAliases: []string{"openid:other-recorder"},
			Mentions:      []onebotBridgeLogMention{{Target: "tinyid:777", Aliases: []string{"openid:cross-bot"}, IsBot: true}},
		})

	markdown, err := onebotBridgeRenderLogSnapshot(d.DBOperator, groupID, "legacy", "md")
	if err != nil {
		t.Fatal(err)
	}
	bodies := onebotBridgeMarkdownRenderedBodies(t, markdown)
	if len(bodies) != 7 {
		t.Fatalf("rendered %d bodies, want 7: %#v", len(bodies), bodies)
	}
	wantOldBody := "historical @机器人 @机器人 unrelated @成员1 nickname @成员2 contested @成员3 shared @成员4 cross @成员5"
	if bodies[0] != wantOldBody {
		t.Fatalf("legacy mention resolution = %q, want %q", bodies[0], wantOldBody)
	}
	if strings.Contains(bodies[3], "@机器人") || !strings.Contains(bodies[3], "@成员") {
		t.Fatalf("human-claimed current-row alias did not fail closed anonymously: %q", bodies[3])
	}
}

func onebotBridgeMarkdownRenderedBodies(t *testing.T, markdown []byte) []string {
	t.Helper()
	divs := regexp.MustCompile(`<div>(.*?)</div>`).FindAllStringSubmatch(string(markdown), -1)
	spanPattern := regexp.MustCompile(`<span style="white-space:pre-wrap;color:#[0-9a-f]{6}">(.*?)</span>`)
	bodies := make([]string, 0, len(divs))
	for _, div := range divs {
		if len(div) != 2 {
			t.Fatalf("malformed Markdown body div: %#v", div)
		}
		spans := spanPattern.FindAllStringSubmatch(div[1], -1)
		if len(spans) == 0 || strings.Count(div[1], "<br />") != len(spans)-1 {
			t.Fatalf("body does not have one colored span per line: %s", div[1])
		}
		var body strings.Builder
		for index, span := range spans {
			if index > 0 {
				body.WriteByte('\n')
			}
			body.WriteString(html.UnescapeString(span[1]))
		}
		bodies = append(bodies, body.String())
	}
	return bodies
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
	for _, want := range []string{"@SDK &lt;Current&gt;", "@Inline", "@Friend &lt;Name&gt;", "@机器人", "@成员1", "@成员2", "成员3", "Author &amp; One"} {
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
