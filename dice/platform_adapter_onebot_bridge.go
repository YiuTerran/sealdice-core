package dice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	socketio "github.com/PaienNate/pineutil/evsocket/v2"
	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	emitter "sealdice-core/dice/imsdk/onebot"
	"sealdice-core/dice/imsdk/onebot/schema"
	"sealdice-core/message"
)

const (
	onebotLLMBridgeRegisterAction = "_llm_bridge_register"
	onebotLLMBridgeCompleteAction = "_llm_bridge_complete"
	onebotBridgeMasterIDMin       = int64(8_000_000_000_000_000)
	onebotBridgeMasterIDMax       = int64(9_000_000_000_000_000)
	onebotBridgeMasterIDLimit     = 100
)

type onebotBridgeConnection struct {
	emitter      emitter.Emitter
	ctx          context.Context
	selfIDOnce   sync.Once
	registerDone chan struct{}
	registerOnce sync.Once

	mu                    sync.RWMutex
	connectionID          string
	registered            bool
	closed                bool
	masterACLConnectionID string
	masterUserIDs         map[int64]struct{}
}

func (c *onebotBridgeConnection) registration() (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connectionID, c.registered
}

func (c *onebotBridgeConnection) setRegistration(connectionID string, registered bool) {
	c.mu.Lock()
	if !c.closed {
		c.connectionID = connectionID
		c.registered = registered
		if !registered || c.masterACLConnectionID != connectionID {
			c.masterACLConnectionID = ""
			c.masterUserIDs = nil
		}
	} else {
		c.connectionID = ""
		c.registered = false
		c.masterACLConnectionID = ""
		c.masterUserIDs = nil
	}
	c.mu.Unlock()
	c.registerOnce.Do(func() {
		if c.registerDone != nil {
			close(c.registerDone)
		}
	})
}

func (c *onebotBridgeConnection) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	c.connectionID = ""
	c.registered = false
	c.masterACLConnectionID = ""
	c.masterUserIDs = nil
	c.mu.Unlock()
	c.registerOnce.Do(func() {
		if c.registerDone != nil {
			close(c.registerDone)
		}
	})
}

func (c *onebotBridgeConnection) setMasterAuthorization(connectionID string, authorization *onebotBridgeAuthorization) {
	if c == nil {
		return
	}
	ids := make(map[int64]struct{})
	valid := authorization != nil && authorization.Version == 1 && strings.TrimSpace(connectionID) != "" && len(authorization.MasterUserIDs) <= onebotBridgeMasterIDLimit
	if valid {
		ids = make(map[int64]struct{}, len(authorization.MasterUserIDs))
		for _, id := range authorization.MasterUserIDs {
			if id < onebotBridgeMasterIDMin || id >= onebotBridgeMasterIDMax {
				valid = false
				break
			}
			ids[id] = struct{}{}
		}
	}
	c.mu.Lock()
	if !c.closed {
		c.masterACLConnectionID = ""
		c.masterUserIDs = nil
		if valid {
			c.masterACLConnectionID = connectionID
			c.masterUserIDs = ids
		}
	}
	c.mu.Unlock()
}

func (c *onebotBridgeConnection) hasMasterUser(connectionID string, userID int64) bool {
	if c == nil || userID <= 0 || connectionID == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed || !c.registered || c.connectionID != connectionID || c.masterACLConnectionID != connectionID {
		return false
	}
	_, ok := c.masterUserIDs[userID]
	return ok
}

func (c *onebotBridgeConnection) authorizedMasterIDs(connectionID string) []int64 {
	if c == nil || connectionID == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed || !c.registered || c.connectionID != connectionID || c.masterACLConnectionID != connectionID {
		return nil
	}
	ids := make([]int64, 0, len(c.masterUserIDs))
	for id := range c.masterUserIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (c *onebotBridgeConnection) waitForRegistration() bool {
	if c == nil {
		return false
	}
	if c.registerDone != nil {
		var ctxDone <-chan struct{}
		if c.ctx != nil {
			ctxDone = c.ctx.Done()
		}
		select {
		case <-c.registerDone:
		case <-ctxDone:
			return false
		}
	}
	_, registered := c.registration()
	return registered
}

type onebotBridgeRegisterParams struct {
	Version         int      `json:"version"`
	BackendInstance string   `json:"backend_instance"`
	Capabilities    []string `json:"capabilities"`
}

type onebotBridgeCompleteParams struct {
	Version         int    `json:"version"`
	SourceMessageID int64  `json:"source_message_id"`
	ConnectionID    string `json:"connection_id"`
	Status          string `json:"status"`
	OutputCount     int    `json:"output_count"`
}

type onebotBridgeRegisterResult struct {
	Version       int             `json:"version"`
	ConnectionID  string          `json:"connection_id"`
	Authorization json.RawMessage `json:"authorization"`
}

type onebotBridgeAuthorization struct {
	Version       int     `json:"version"`
	MasterUserIDs []int64 `json:"master_user_ids"`
}

type onebotActionResponse struct {
	Status  string          `json:"status"`
	RetCode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
}

// onebotBridgeRequestTracker is created for one registered socket and one
// synthetic OneBot message. Its connection and recipient identities never
// change, including when the adapter reconnects while the request is running.
type onebotBridgeRequestTracker struct {
	adapter       *PlatformAdapterOnebot
	connection    *onebotBridgeConnection
	connectionID  string
	sourceMessage int64
	audience      string
	userID        int64
	groupID       int64

	mu              sync.Mutex
	tasks           int
	commandSolved   bool
	failed          bool
	failureStage    string
	outputCount     int
	completionFired bool
}

func newOnebotBridgeRequestTracker(p *PlatformAdapterOnebot, conn *onebotBridgeConnection, msg *Message) (*onebotBridgeRequestTracker, error) {
	if p == nil || conn == nil || msg == nil {
		return nil, errors.New("bridge request is missing its adapter, connection, or message")
	}
	connectionID, registered := conn.registration()
	if !registered || connectionID == "" {
		return nil, errors.New("bridge connection has not registered")
	}
	sourceMessageID, ok := onebotPositiveInt32(msg.RawID)
	if !ok {
		return nil, errors.New("bridge source message id is invalid")
	}
	userID := ExtractQQEmitterUserID(msg.Sender.UserID)
	tracker := &onebotBridgeRequestTracker{
		adapter:       p,
		connection:    conn,
		connectionID:  connectionID,
		sourceMessage: sourceMessageID,
		audience:      msg.MessageType,
		userID:        userID,
		tasks:         1,
	}
	if userID <= 0 {
		tracker.markFailed()
		return tracker, errors.New("bridge sender id is invalid")
	}
	tracker.userID = userID
	switch msg.MessageType {
	case "group":
		tracker.groupID = ExtractQQEmitterGroupID(msg.GroupID)
		if tracker.groupID <= 0 {
			tracker.markFailed()
			return tracker, errors.New("bridge group id is invalid")
		}
	case "private":
	default:
		tracker.markFailed()
		return tracker, errors.New("bridge message type is unsupported")
	}
	return tracker, nil
}

func onebotPositiveInt32(value any) (int64, bool) {
	var parsed int64
	switch v := value.(type) {
	case int:
		parsed = int64(v)
	case int32:
		parsed = int64(v)
	case int64:
		parsed = v
	case uint:
		if uint64(v) > math.MaxInt32 {
			return 0, false
		}
		parsed = int64(v)
	case uint32:
		parsed = int64(v)
	case uint64:
		if v > math.MaxInt32 {
			return 0, false
		}
		parsed = int64(v)
	case json.Number:
		vInt, err := v.Int64()
		if err != nil {
			return 0, false
		}
		parsed = vInt
	case string:
		vInt, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = vInt
	default:
		return 0, false
	}
	return parsed, parsed > 0 && parsed <= math.MaxInt32
}

func (r *onebotBridgeRequestTracker) beginTask() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.completionFired {
		r.mu.Unlock()
		return false
	}
	r.tasks++
	r.mu.Unlock()
	return true
}

func (r *onebotBridgeRequestTracker) finishTask() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.tasks > 0 {
		r.tasks--
	}
	shouldComplete := r.tasks == 0 && !r.completionFired
	if shouldComplete {
		r.completionFired = true
	}
	status := "failed"
	if !r.failed && r.commandSolved {
		status = "ok"
	}
	failureStage := r.failureStage
	if status == "failed" && failureStage == "" {
		failureStage = "execution_failed"
	}
	params := onebotBridgeCompleteParams{
		Version:         1,
		SourceMessageID: r.sourceMessage,
		ConnectionID:    r.connectionID,
		Status:          status,
		OutputCount:     r.outputCount,
	}
	r.mu.Unlock()

	if shouldComplete {
		if status == "failed" && r.adapter != nil && r.adapter.logger != nil {
			r.adapter.logger.Warnf("OneBot LLM bridge request failed: source_message_id=%d stage=%s output_count=%d", r.sourceMessage, failureStage, params.OutputCount)
		}
		r.emitCompletion(params)
	}
}

func (r *onebotBridgeRequestTracker) markFailed() {
	r.markFailedAt("execution_failed")
}

func (r *onebotBridgeRequestTracker) markFailedAt(stage string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.failed = true
	if r.failureStage == "" {
		r.failureStage = stage
	}
	r.mu.Unlock()
}

func (r *onebotBridgeRequestTracker) markCommandResult(solved bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if solved {
		r.commandSolved = true
	} else {
		r.failed = true
		if r.failureStage == "" {
			r.failureStage = "native_command_unsolved"
		}
	}
	r.mu.Unlock()
}

func (r *onebotBridgeRequestTracker) recordSend(success bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if success {
		r.outputCount++
	} else {
		r.failed = true
		if r.failureStage == "" {
			r.failureStage = "send_failed"
		}
	}
	r.mu.Unlock()
}

func (r *onebotBridgeRequestTracker) emitCompletion(params onebotBridgeCompleteParams) {
	if r == nil || r.connection == nil || r.connection.emitter == nil {
		return
	}
	ctx := r.connection.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := r.connection.emitter.Raw(ctx, onebotLLMBridgeCompleteAction, params); err != nil {
		// The emitter includes action parameters in its error string. These fields
		// contain only protocol IDs and state, so logging the action without err is
		// still bounded and cannot expose private message text.
		if r.adapter.logger != nil {
			r.adapter.logger.Warnf("OneBot LLM bridge completion action failed: source_message_id=%d", r.sourceMessage)
		}
	}
}

func (p *PlatformAdapterOnebot) bridgeConnection(kws *socketio.WebsocketWrapper) *onebotBridgeConnection {
	if p == nil || kws == nil {
		return nil
	}
	p.bridgeConnectionMu.RLock()
	conn := p.bridgeConnections[kws]
	p.bridgeConnectionMu.RUnlock()
	return conn
}

func (p *PlatformAdapterOnebot) registerLLMBridgeConnection(conn *onebotBridgeConnection) error {
	if p == nil || conn == nil || conn.emitter == nil {
		return errors.New("bridge emitter unavailable")
	}
	p.bridgeInstanceOnce.Do(func() {
		p.bridgeInstanceID = uuid.NewString()
	})
	params := onebotBridgeRegisterParams{
		Version:         1,
		BackendInstance: p.bridgeInstanceID,
		Capabilities:    []string{"reply", "complete", "master-acl-v1"},
	}
	ctx := conn.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	data, err := conn.emitter.Raw(ctx, onebotLLMBridgeRegisterAction, params)
	if err != nil {
		return fmt.Errorf("register bridge action failed: %w", err)
	}
	var response onebotActionResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return errors.New("register bridge action returned malformed response")
	}
	if strings.EqualFold(response.Status, "failed") || response.RetCode != 0 {
		return fmt.Errorf("register bridge action rejected: status=%s retcode=%d", response.Status, response.RetCode)
	}
	var result onebotBridgeRegisterResult
	if err := json.Unmarshal(response.Data, &result); err != nil {
		return errors.New("register bridge action returned malformed data")
	}
	if result.Version != 1 || strings.TrimSpace(result.ConnectionID) == "" {
		return errors.New("register bridge action returned invalid connection identity")
	}
	var authorization *onebotBridgeAuthorization
	if len(result.Authorization) != 0 && string(result.Authorization) != "null" {
		var parsed onebotBridgeAuthorization
		if json.Unmarshal(result.Authorization, &parsed) == nil {
			authorization = &parsed
		}
	}
	conn.setMasterAuthorization(result.ConnectionID, authorization)
	conn.setRegistration(result.ConnectionID, true)
	if _, registered := conn.registration(); !registered {
		return errors.New("bridge connection closed during registration")
	}
	return nil
}

func (p *PlatformAdapterOnebot) sendBridgeText(ctx *MsgContext, groupID, userID string, segments []message.IMessageElement) {
	tracker := (*onebotBridgeRequestTracker)(nil)
	if ctx != nil {
		tracker = ctx.LLMBridgeRequest
	}
	if tracker == nil || tracker.connection == nil || tracker.connection.emitter == nil {
		if p != nil && p.logger != nil {
			p.logger.Warn("OneBot LLM bridge output rejected: missing request context")
		}
		return
	}
	if len(segments) == 0 {
		return
	}
	var targetID int64
	isGroup := groupID != ""
	if isGroup {
		targetID = ExtractQQEmitterGroupID(groupID)
		if tracker.audience != "group" || targetID <= 0 || targetID != tracker.groupID {
			tracker.recordSend(false)
			if p.logger != nil {
				p.logger.Warnf("OneBot LLM bridge output rejected: source_message_id=%d audience=group", tracker.sourceMessage)
			}
			return
		}
	} else {
		targetID = ExtractQQEmitterUserID(userID)
		if targetID <= 0 || targetID != tracker.userID {
			tracker.recordSend(false)
			if p.logger != nil {
				p.logger.Warnf("OneBot LLM bridge output rejected: source_message_id=%d audience=private", tracker.sourceMessage)
			}
			return
		}
	}
	for _, segment := range segments {
		if segment == nil || segment.Type() != message.Text {
			tracker.recordSend(false)
			if p.logger != nil {
				p.logger.Warnf("OneBot LLM bridge output rejected: source_message_id=%d unsupported_segment", tracker.sourceMessage)
			}
			return
		}
	}
	tracker.mu.Lock()
	canSend := !tracker.completionFired && tracker.tasks > 0
	tracker.mu.Unlock()
	if !canSend {
		tracker.recordSend(false)
		return
	}
	chain, _ := convertSealMsgToMessageChain(segments)
	replyData, err := json.Marshal(schema.Reply{Id: int(tracker.sourceMessage)})
	if err != nil {
		tracker.recordSend(false)
		return
	}
	chain = append(schema.MessageChain{{Type: "reply", Data: sonic.NoCopyRawMessage(replyData)}}, chain...)
	ctxForSend := tracker.connection.ctx
	if ctxForSend == nil {
		ctxForSend = context.Background()
	}
	if isGroup {
		_, err = tracker.connection.emitter.SendGrMsg(ctxForSend, targetID, chain)
	} else {
		_, err = tracker.connection.emitter.SendPvtMsg(ctxForSend, targetID, chain)
	}
	tracker.recordSend(err == nil)
	if err != nil && p.logger != nil {
		if isGroup {
			p.logger.Warnf("OneBot LLM bridge group send failed: source_message_id=%d", tracker.sourceMessage)
		} else {
			// SendPvtMsg errors include the serialized action parameters, including
			// private message text. Never log or wrap that value on this path.
			p.logger.Warnf("OneBot LLM bridge private send failed: source_message_id=%d", tracker.sourceMessage)
		}
	}
}
