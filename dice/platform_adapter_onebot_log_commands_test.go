//nolint:testpackage // These tests exercise private bridge command and parser state.
package dice

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"sealdice-core/dice/service"
	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

func TestOnebotBridgeLogCommandShapeMatrix(t *testing.T) {
	allowed := []struct {
		raw    string
		action string
		name   string
		format string
	}{
		{raw: ".log new", action: "new", format: "md"},
		{raw: ".log new session_一", action: "new", name: "session_一", format: "md"},
		{raw: ".log on session", action: "on", name: "session", format: "md"},
		{raw: ".log off", action: "off", format: "md"},
		{raw: ".log halt", action: "halt", format: "md"},
		{raw: ".log end", action: "end", format: "md"},
		{raw: ".log list", action: "list", format: "md"},
		{raw: ".log stat session", action: "stat", name: "session", format: "md"},
		{raw: ".log get", action: "get", format: "md"},
		{raw: ".log get session --format=txt", action: "get", name: "session", format: "txt"},
		{raw: ".log get --format=txt", action: "get", format: "txt"},
		{raw: ".log export session --format=txt", action: "export", name: "session", format: "txt"},
		{raw: ".log export --format=txt", action: "export", format: "txt"},
		{raw: ".log del session", action: "del", name: "session", format: "md"},
	}
	for _, testCase := range allowed {
		t.Run(testCase.raw, func(t *testing.T) {
			got := onebotBridgeParseLogCommand(testCase.raw)
			if got == nil || got.action != testCase.action || got.name != testCase.name || got.format != testCase.format {
				t.Fatalf("parse(%q) = %#v", testCase.raw, got)
			}
			ctx := &MsgContext{MessageType: "group"}
			args := &CmdArgs{Command: "log", RawText: testCase.raw}
			if !onebotBridgeCommandAllowed(ctx, "log", args) {
				t.Fatalf("valid command rejected: %q", testCase.raw)
			}
		})
	}
	denied := []string{
		".log list 8000000000000001", ".log masterget 8000000000000001 x", ".log export user@example.com",
		".log get --format=md", ".log get --all", ".log new one two", ".log on --format=txt", ".log del",
		".log end extra", ".log get name --format=txt extra", ".log new --name", ".log new [CQ:at,qq=1]",
		".log new " + strings.Repeat("界", 81), ".log new\nlist", ".log  list", ".log list ",
	}
	for _, raw := range denied {
		if got := onebotBridgeParseLogCommand(raw); got != nil {
			t.Errorf("parse(%q) = %#v, want rejection", raw, got)
		}
	}
}

func TestOnebotBridgeLogReadOnlyVsMutationRoleMatrix(t *testing.T) {
	for _, role := range []string{"owner", "admin", "member", ""} {
		conn := &onebotBridgeConnection{registerDone: make(chan struct{})}
		conn.setRegistration("log-policy", true)
		msg := &Message{MessageType: "group", RawID: int64(42), Sender: SenderBase{UserID: "QQ:8000000000000002", GroupRole: role}, GroupID: "QQ-Group:8000000000000001"}
		tracker, err := newOnebotBridgeRequestTracker(&PlatformAdapterOnebot{}, conn, msg)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{".log list", ".log stat story", ".log get", ".log export --format=txt"} {
			args := &CmdArgs{Command: "log", RawText: raw}
			if onebotBridgeCommandRequiresGroupAdmin(nil, "log", args) {
				t.Errorf("read-only %q unexpectedly requires admin for role %q", raw, role)
			}
			if !onebotBridgeIsReadOnlyCommand("log", args) {
				t.Errorf("read-only command %q not classified read-only", raw)
			}
		}
		for _, raw := range []string{".log new story", ".log on story", ".log off", ".log halt", ".log end", ".log del story"} {
			args := &CmdArgs{Command: "log", RawText: raw, Args: []string{rawVerb(raw)}}
			if !onebotBridgeCommandRequiresGroupAdmin(nil, "log", args) {
				t.Errorf("mutation %q did not require admin for role %q", raw, role)
			}
			allowed, _ := tracker.groupRoleAuthorization()
			wantAllowed := role == "owner" || role == "admin"
			if allowed != wantAllowed {
				t.Errorf("groupRoleAuthorization role=%q = %v, want %v", role, allowed, wantAllowed)
			}
		}
	}
}

func rawVerb(raw string) string {
	body := strings.TrimPrefix(raw, ".log ")
	verb, _, _ := strings.Cut(body, " ")
	return verb
}

func TestOnebotBridgeLogExecuteNewLifecycle(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	d.onebotBridgeIsolated = true
	d.onebotBridgeBind = "0.0.0.0:18081"
	d.onebotBridgeToken = "runtime-only"
	d.ExtList = nil
	d.ExtRegistry = new(SyncMap[string, *ExtInfo])
	d.registerBuiltinExtForRuntime()
	d.applyOnebotBridgeIsolation()
	if d.ExtFind("log", false) != nil {
		t.Fatal("isolated bootstrap registered the legacy log extension")
	}
	parserExt := d.ExtFind("bridge-log", false)
	if parserExt == nil || len(parserExt.GetCmdMap()) != 1 || parserExt.GetCmdMap()["log"] == nil || parserExt.GetCmdMap()["log"].Solve != nil || parserExt.OnMessageReceived != nil || parserExt.OnMessageSend != nil {
		t.Fatalf("isolated bridge log parser has executable hooks: %#v", parserExt)
	}
	var ep *EndPointInfo
	for _, candidate := range d.ImSession.EndPoints {
		if adapter, ok := candidate.Adapter.(*PlatformAdapterOnebot); ok && adapter.LLMBridgeEnabled {
			ep = candidate
			break
		}
	}
	if ep == nil {
		t.Fatal("isolated bridge endpoint missing")
	}
	ep.UserID = "QQ:10000"
	pa := ep.Adapter.(*PlatformAdapterOnebot)
	pa.logger = d.Logger
	if err := d.DBOperator.GetDataDB(constant.WRITE).AutoMigrate(&model.GroupInfo{}, &model.GroupPlayerInfoBase{}); err != nil {
		t.Fatal(err)
	}
	if err := d.DBOperator.GetLogDB(constant.WRITE).AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}, &model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
		t.Fatal(err)
	}
	em := &onebotBridgeCaptureEmitter{completeCh: make(chan onebotBridgeCompleteParams, 8)}
	const groupNumber int64 = 8000000000000011
	const userNumber int64 = 8000000000000012
	groupID := fmt.Sprintf("QQ-Group:%d", groupNumber)

	run := func(sourceID int64, role, command, wantStatus string, wantOutput int) {
		t.Helper()
		conn, tracker := newOnebotBridgeTestTracker(t, pa, em, "group", sourceID, userNumber, groupNumber, fmt.Sprintf("log-command-%d", sourceID), role)
		defer conn.close()
		msg := newGroupMsg(groupID, fmt.Sprintf("QQ:%d", userNumber), command)
		msg.RawID = sourceID
		msg.LLMBridgeRequest = tracker
		d.ImSession.ExecuteNew(ep, msg)
		tracker.finishTask()
		select {
		case completion := <-em.completeCh:
			if completion.SourceMessageID != sourceID || completion.Status != wantStatus || completion.OutputCount != wantOutput {
				t.Fatalf("%q role=%s completion=%#v, want %s with %d outputs", command, role, completion, wantStatus, wantOutput)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%q did not complete", command)
		}
	}

	run(701, "member", ".log new story", "failed", 0)
	if _, err := service.OnebotBridgeLogStateGet(d.DBOperator, groupID); err == nil {
		t.Fatal("member created a native log")
	}
	run(702, "admin", ".log new story", "ok", 1)
	state, err := service.OnebotBridgeLogStateGet(d.DBOperator, groupID)
	if err != nil || !state.On || state.Name != "story" {
		t.Fatalf("admin new state=%#v err=%v", state, err)
	}
	if _, _, appendErr := service.LogCaptureAppend(d.DBOperator, "native-entry", groupID, "message", &model.LogOneItem{
		Nickname: "Member", IMUserID: strconv.FormatInt(userNumber, 10), Time: 1710000001, Message: "native captured body",
	}); appendErr != nil {
		t.Fatal(appendErr)
	}
	run(703, "member", ".log list", "ok", 1)
	run(704, "member", ".log get story", "ok", 1)
	run(705, "member", ".log off", "failed", 0)
	state, err = service.OnebotBridgeLogStateGet(d.DBOperator, groupID)
	if err != nil || !state.On {
		t.Fatalf("member changed recording state=%#v err=%v", state, err)
	}
	run(706, "owner", ".log off", "ok", 1)
	state, err = service.OnebotBridgeLogStateGet(d.DBOperator, groupID)
	if err != nil || state.On {
		t.Fatalf("owner off state=%#v err=%v", state, err)
	}
	items, err := service.LogGetAllLines(d.DBOperator, groupID, "story")
	if err != nil || len(items) != 1 || items[0].Message != "native captured body" {
		t.Fatalf("virtual commands entered native log: items=%#v err=%v", items, err)
	}
}
