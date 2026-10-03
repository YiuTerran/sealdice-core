//nolint:testpackage // These tests exercise private tracker and emitter lifecycle state.
package dice

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	socketio "github.com/PaienNate/pineutil/evsocket/v2"
	"github.com/bytedance/sonic"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	emitter "sealdice-core/dice/imsdk/onebot"
	"sealdice-core/dice/imsdk/onebot/schema"
	emitterTypes "sealdice-core/dice/imsdk/onebot/types"
	"sealdice-core/message"
)

type onebotBridgeCapturedSend struct {
	audience string
	targetID int64
	chain    schema.MessageChain
}

type onebotBridgeCapturedCompletion struct {
	action string
	params onebotBridgeCompleteParams
}

type onebotBridgeCaptureEmitter struct {
	emitter.Emitter

	mu          sync.Mutex
	sends       []onebotBridgeCapturedSend
	completions []onebotBridgeCapturedCompletion
	sendErr     error
	sendStarted chan struct{}
	releaseSend chan struct{}
	completeCh  chan onebotBridgeCompleteParams
}

var _ emitter.Emitter = (*onebotBridgeCaptureEmitter)(nil)

func (e *onebotBridgeCaptureEmitter) captureSend(audience string, targetID int64, chain schema.MessageChain) error {
	copyChain := make(schema.MessageChain, len(chain))
	for i, part := range chain {
		copyChain[i] = part
		copyChain[i].Data = append(sonic.NoCopyRawMessage(nil), part.Data...)
	}
	e.mu.Lock()
	e.sends = append(e.sends, onebotBridgeCapturedSend{audience: audience, targetID: targetID, chain: copyChain})
	err := e.sendErr
	e.mu.Unlock()
	if e.sendStarted != nil {
		select {
		case e.sendStarted <- struct{}{}:
		default:
		}
	}
	if e.releaseSend != nil {
		<-e.releaseSend
	}
	return err
}

func (e *onebotBridgeCaptureEmitter) SendPvtMsg(_ context.Context, userID int64, chain schema.MessageChain) (*emitterTypes.SendMsgRes, error) {
	return &emitterTypes.SendMsgRes{}, e.captureSend("private", userID, chain)
}

func (e *onebotBridgeCaptureEmitter) SendGrMsg(_ context.Context, groupID int64, chain schema.MessageChain) (*emitterTypes.SendMsgRes, error) {
	return &emitterTypes.SendMsgRes{}, e.captureSend("group", groupID, chain)
}

func (e *onebotBridgeCaptureEmitter) Raw(_ context.Context, action emitter.Action, params any) ([]byte, error) {
	if action == onebotLLMBridgeRegisterAction {
		return []byte(`{"status":"ok","retcode":0,"data":{"version":1,"connection_id":"registered-connection"}}`), nil
	}
	if action != onebotLLMBridgeCompleteAction {
		return []byte(`{"status":"ok","retcode":0,"data":{}}`), nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var completion onebotBridgeCompleteParams
	if err := json.Unmarshal(encoded, &completion); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.completions = append(e.completions, onebotBridgeCapturedCompletion{action: action, params: completion})
	e.mu.Unlock()
	if e.completeCh != nil {
		e.completeCh <- completion
	}
	return []byte(`{"status":"ok","retcode":0,"data":{}}`), nil
}

func (e *onebotBridgeCaptureEmitter) HandleEcho(emitter.Response[sonic.NoCopyRawMessage]) {}
func (e *onebotBridgeCaptureEmitter) GetDroppedEchoCount() uint64                         { return 0 }

func newOnebotBridgeTestTracker(t *testing.T, p *PlatformAdapterOnebot, em *onebotBridgeCaptureEmitter, audience string, sourceID, userID, groupID int64, connectionID string) (*onebotBridgeConnection, *onebotBridgeRequestTracker) {
	t.Helper()
	conn := &onebotBridgeConnection{
		emitter:      em,
		ctx:          context.Background(),
		registerDone: make(chan struct{}),
	}
	conn.setRegistration(connectionID, true)
	msg := &Message{
		MessageType: audience,
		RawID:       sourceID,
		Sender:      SenderBase{UserID: "QQ:" + strconvFormatInt(userID)},
	}
	if audience == "group" {
		msg.GroupID = "QQ-Group:" + strconvFormatInt(groupID)
	}
	tracker, err := newOnebotBridgeRequestTracker(p, conn, msg)
	if err != nil {
		t.Fatalf("newOnebotBridgeRequestTracker() error = %v", err)
	}
	return conn, tracker
}

func strconvFormatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestOnebotBridgeRegistrationRaceAndNativeCompletionAfterSend(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	logCore, observedLogs := observer.New(zapcore.DebugLevel)
	d.Logger = zap.New(logCore).Sugar()

	em := &onebotBridgeCaptureEmitter{
		sendStarted: make(chan struct{}, 1),
		releaseSend: make(chan struct{}),
		completeCh:  make(chan onebotBridgeCompleteParams, 1),
	}
	pa := &PlatformAdapterOnebot{
		EndPoint:         ep,
		LLMBridgeEnabled: true,
		logger:           d.Logger,
	}
	ep.Adapter = pa
	ep.Session = d.ImSession
	kws := &socketio.WebsocketWrapper{}
	conn := &onebotBridgeConnection{emitter: em, ctx: context.Background(), registerDone: make(chan struct{})}
	pa.bridgeConnections = map[*socketio.WebsocketWrapper]*onebotBridgeConnection{kws: conn}

	// The event is dispatched immediately after the peer has written the
	// registration ACK, while this process has not yet consumed its action echo.
	const raw = `{"post_type":"message","message_type":"group","self_id":10000,"user_id":20001,"group_id":10001,"message_id":345,"raw_message":".r 1d1","message":[{"type":"text","data":{"text":".r 1d1"}}],"sender":{"user_id":20001,"nickname":"Alice","role":"member"}}`
	pa.onOnebotMessageEvent(&socketio.EventPayload{Kws: kws, Data: []byte(raw)})
	select {
	case <-em.sendStarted:
		t.Fatal("native command ran before registration result was consumed")
	case <-time.After(20 * time.Millisecond):
	}
	conn.setRegistration("connection-race", true)
	select {
	case <-em.sendStarted:
	case got := <-em.completeCh:
		t.Fatalf("request completed before native send: %#v logs=%v", got, observedLogs.All())
	case <-time.After(3 * time.Second):
		t.Fatalf("native command did not send after registration completed; logs=%v", observedLogs.All())
	}
	select {
	case got := <-em.completeCh:
		t.Fatalf("completion fired before the blocked send returned: %#v", got)
	default:
	}
	close(em.releaseSend)
	select {
	case got := <-em.completeCh:
		if got.SourceMessageID != 345 || got.ConnectionID != "connection-race" || got.Status != "ok" || got.OutputCount != 1 {
			t.Fatalf("unexpected native completion: %#v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native request did not complete after successful send")
	}

	em.mu.Lock()
	defer em.mu.Unlock()
	if len(em.sends) == 0 {
		t.Fatal("native .r 1d1 command produced no sends")
	}
	for _, sent := range em.sends {
		assertOnebotBridgeReply(t, sent.chain, 345)
	}
}

func TestOnebotBridgeSplitAndPrivateSendsAreCorrelatedAndOutOfOrderSafe(t *testing.T) {
	p := &PlatformAdapterOnebot{LLMBridgeEnabled: true, logger: zap.NewNop().Sugar()}
	oldEmitter := &onebotBridgeCaptureEmitter{completeCh: make(chan onebotBridgeCompleteParams, 1)}
	newEmitter := &onebotBridgeCaptureEmitter{completeCh: make(chan onebotBridgeCompleteParams, 1)}
	_, first := newOnebotBridgeTestTracker(t, p, oldEmitter, "group", 451, 20001, 10001, "old-connection")
	_, second := newOnebotBridgeTestTracker(t, p, newEmitter, "private", 452, 20002, 0, "new-connection")
	firstCtx := &MsgContext{LLMBridgeRequest: first}

	p.sendEmitter = newEmitter // later/global state must not affect first's captured emitter
	p.SendSegmentToGroup(firstCtx, "QQ-Group:10001", message.ConvertStringMessage("group one"), "")
	p.SendSegmentToGroup(firstCtx, "QQ-Group:10001", message.ConvertStringMessage("group two"), "")
	p.SendSegmentToPerson(firstCtx, "QQ:20001", message.ConvertStringMessage("private receipt"), "")
	first.markCommandResult(true)
	first.finishTask()
	second.markCommandResult(true)
	second.finishTask()

	completion2 := <-newEmitter.completeCh
	completion1 := <-oldEmitter.completeCh
	if completion2.ConnectionID != "new-connection" || completion2.SourceMessageID != 452 || completion2.OutputCount != 0 {
		t.Fatalf("unexpected second completion: %#v", completion2)
	}
	if completion1.ConnectionID != "old-connection" || completion1.SourceMessageID != 451 || completion1.OutputCount != 3 {
		t.Fatalf("unexpected first completion: %#v", completion1)
	}

	oldEmitter.mu.Lock()
	if len(oldEmitter.sends) != 3 {
		oldEmitter.mu.Unlock()
		t.Fatalf("captured sends on original emitter = %d, want 3", len(oldEmitter.sends))
	}
	for _, sent := range oldEmitter.sends {
		assertOnebotBridgeReply(t, sent.chain, 451)
	}
	oldEmitter.mu.Unlock()
	newEmitter.mu.Lock()
	if len(newEmitter.sends) != 0 {
		newEmitter.mu.Unlock()
		t.Fatalf("later emitter received %d output sends, want 0", len(newEmitter.sends))
	}
	newEmitter.mu.Unlock()
}

func TestOnebotBridgePrivateSendFailureIsSanitizedAndCompletesFailed(t *testing.T) {
	core, observed := observer.New(zapcore.WarnLevel)
	p := &PlatformAdapterOnebot{LLMBridgeEnabled: true, logger: zap.New(core).Sugar()}
	em := &onebotBridgeCaptureEmitter{
		sendErr:    errors.New("private-body-must-not-be-logged"),
		completeCh: make(chan onebotBridgeCompleteParams, 1),
	}
	_, tracker := newOnebotBridgeTestTracker(t, p, em, "private", 501, 20001, 0, "private-connection")
	p.SendSegmentToPerson(&MsgContext{LLMBridgeRequest: tracker}, "QQ:20001", message.ConvertStringMessage("private-body-must-not-be-logged"), "")
	tracker.markCommandResult(true)
	tracker.finishTask()
	completion := <-em.completeCh
	if completion.Status != "failed" || completion.OutputCount != 0 {
		t.Fatalf("unexpected failed-send completion: %#v", completion)
	}
	for _, entry := range observed.All() {
		if strings.Contains(entry.Message, "private-body-must-not-be-logged") {
			t.Fatalf("private body leaked to log: %q", entry.Message)
		}
	}
}

func TestOnebotBridgeMalformedEventDoesNotLogPayload(t *testing.T) {
	core, observed := observer.New(zapcore.WarnLevel)
	pa := &PlatformAdapterOnebot{LLMBridgeEnabled: true, logger: zap.New(core).Sugar()}
	conn := &onebotBridgeConnection{
		emitter:      &onebotBridgeCaptureEmitter{},
		ctx:          context.Background(),
		registerDone: make(chan struct{}),
	}
	conn.setRegistration("malformed-connection", true)
	const sentinel = "hidden-command-and-identity-sentinel"
	raw := []byte(`{"post_type":"message","message_type":"private","message_id":"` + sentinel + `","user_id":"` + sentinel + `","message":"` + sentinel + `"}`)
	pa.processOnebotMessageEvent(raw, conn)
	for _, entry := range observed.All() {
		if strings.Contains(entry.Message, sentinel) {
			t.Fatalf("malformed bridge payload leaked to log: %q", entry.Message)
		}
	}
}

func TestOnebotBridgeSolvedNoOutputIsOkAndLateTaskCannotRestart(t *testing.T) {
	p := &PlatformAdapterOnebot{LLMBridgeEnabled: true, logger: zap.NewNop().Sugar()}
	em := &onebotBridgeCaptureEmitter{completeCh: make(chan onebotBridgeCompleteParams, 1)}
	_, tracker := newOnebotBridgeTestTracker(t, p, em, "private", 601, 20001, 0, "no-output")
	if !tracker.beginTask() {
		t.Fatal("beginTask() = false before completion")
	}
	tracker.markCommandResult(true)
	tracker.finishTask()
	select {
	case got := <-em.completeCh:
		t.Fatalf("completion fired while another task was outstanding: %#v", got)
	default:
	}
	tracker.finishTask()
	select {
	case got := <-em.completeCh:
		if got.Status != "ok" || got.OutputCount != 0 {
			t.Fatalf("solved no-output completion = %#v, want ok with zero outputs", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no-output command did not get a terminal completion")
	}
	if tracker.beginTask() {
		t.Fatal("beginTask() succeeded after terminal completion")
	}
}

func TestOnebotBridgeOrdinaryEndpointKeepsUncorrelatedSendBehavior(t *testing.T) {
	em := &onebotBridgeCaptureEmitter{}
	p := &PlatformAdapterOnebot{sendEmitter: em, EndPoint: &EndPointInfo{}}
	p.SendSegmentToGroup(nil, "QQ-Group:10001", message.ConvertStringMessage("ordinary"), "")
	em.mu.Lock()
	defer em.mu.Unlock()
	if len(em.sends) != 1 {
		t.Fatalf("ordinary send count = %d, want 1", len(em.sends))
	}
	if len(em.sends[0].chain) == 0 || em.sends[0].chain[0].Type == "reply" {
		t.Fatalf("ordinary endpoint unexpectedly added bridge correlation: %#v", em.sends[0].chain)
	}
}

func assertOnebotBridgeReply(t *testing.T, chain schema.MessageChain, sourceID int64) {
	t.Helper()
	if len(chain) < 2 || chain[0].Type != "reply" {
		t.Fatalf("bridge send does not start with reply segment: %#v", chain)
	}
	var reply schema.Reply
	if err := json.Unmarshal(chain[0].Data, &reply); err != nil {
		t.Fatalf("decode reply segment: %v", err)
	}
	if int64(reply.Id) != sourceID {
		t.Fatalf("reply id = %d, want %d", reply.Id, sourceID)
	}
}
