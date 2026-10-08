package dice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"sealdice-core/dice/service"
	"sealdice-core/model"
	engine2 "sealdice-core/utils/dboperator/engine"
)

const (
	onebotLLMBridgeLogEventPostType = "_llm_bridge_log_event"
	onebotBridgeArtifactMaxBytes    = 10 * 1024 * 1024
	onebotBridgeCaptureMaxText      = 1024 * 1024
)

var (
	errOnebotBridgeLogAckActionFailed   = errors.New("OneBot bridge log ACK action failed")
	errOnebotBridgeArtifactActionFailed = errors.New("OneBot bridge artifact action failed")
	errOnebotBridgeArtifactTooLarge     = errors.New("log artifact exceeds 10 MiB")
)

type onebotBridgeLogEvent struct {
	PostType     string          `json:"post_type"`
	Version      int             `json:"version"`
	ConnectionID string          `json:"connection_id"`
	EventID      string          `json:"event_id"`
	GroupID      int64           `json:"group_id"`
	UserID       int64           `json:"user_id"`
	Time         int64           `json:"time"`
	Nickname     string          `json:"nickname"`
	Text         string          `json:"text"`
	IsBot        *bool           `json:"is_bot"`
	Kind         string          `json:"kind"`
	Display      json.RawMessage `json:"display,omitempty"`
}

type onebotBridgeLogDisplay struct {
	AuthorAliases []string                 `json:"author_aliases"`
	Mentions      []onebotBridgeLogMention `json:"mentions"`
}

type onebotBridgeLogMention struct {
	Target  string   `json:"target"`
	Aliases []string `json:"aliases,omitempty"`
	Name    string   `json:"name,omitempty"`
	IsBot   bool     `json:"is_bot,omitempty"`
}

type onebotBridgeLogAckParams struct {
	Version      int    `json:"version"`
	ConnectionID string `json:"connection_id"`
	EventID      string `json:"event_id"`
	Status       string `json:"status"`
}

type onebotBridgeArtifactParams struct {
	Version         int    `json:"version"`
	ConnectionID    string `json:"connection_id"`
	SourceMessageID int64  `json:"source_message_id"`
	Filename        string `json:"filename"`
	MediaType       string `json:"media_type"`
	BytesBase64     string `json:"bytes_base64"`
}

func (p *PlatformAdapterOnebot) processOnebotBridgeLogEvent(raw []byte, conn *onebotBridgeConnection) {
	if p == nil || conn == nil || !p.LLMBridgeEnabled {
		return
	}
	connectionID, registered := conn.registration()
	if !registered || connectionID == "" {
		return
	}
	var event onebotBridgeLogEvent
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		if p.logger != nil {
			p.logger.Warn("OneBot bridge log event rejected: invalid frame")
		}
		return
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if p.logger != nil {
			p.logger.Warn("OneBot bridge log event rejected: invalid frame")
		}
		return
	}
	if event.ConnectionID != connectionID || event.PostType != onebotLLMBridgeLogEventPostType {
		return
	}
	if err := validateOnebotBridgeLogEvent(&event); err != nil {
		if validOnebotBridgeEventID(event.EventID) {
			_ = p.emitOnebotBridgeLogAck(conn, connectionID, event.EventID, "failed")
		}
		if p.logger != nil {
			p.logger.Warn("OneBot bridge log event rejected: invalid fields")
		}
		return
	}
	var display *onebotBridgeLogDisplay
	if conn.hasLogDisplayCapability(connectionID) {
		display = parseOnebotBridgeLogDisplay(event.Display)
	}
	if p.EndPoint == nil || p.EndPoint.Session == nil || p.EndPoint.Session.Parent == nil {
		_ = p.emitOnebotBridgeLogAck(conn, connectionID, event.EventID, "failed")
		return
	}
	commandInfo := map[string]interface{}{"bridgeCaptureKind": event.Kind}
	if display != nil {
		commandInfo["bridgeDisplay"] = display
	}
	item := &model.LogOneItem{
		Nickname:    event.Nickname,
		IMUserID:    strconv.FormatInt(event.UserID, 10),
		UniformID:   fmt.Sprintf("OneBotBridge:QQ:%d", event.UserID),
		Time:        event.Time,
		Message:     event.Text,
		IsDice:      *event.IsBot,
		RawMsgID:    event.EventID,
		CommandInfo: commandInfo,
	}
	if *event.IsBot {
		item.UniformID = fmt.Sprintf("OneBotBridge:bot:%d", event.UserID)
	}
	groupID := fmt.Sprintf("QQ-Group:%d", event.GroupID)
	_, _, err := service.LogCaptureAppend(p.EndPoint.Session.Parent.DBOperator, event.EventID, groupID, event.Kind, item)
	status := "ok"
	if err != nil {
		status = "failed"
		if p.logger != nil {
			p.logger.Warn("OneBot bridge log event persistence failed")
		}
	}
	if err := p.emitOnebotBridgeLogAck(conn, connectionID, event.EventID, status); err != nil {
		if p.logger != nil {
			p.logger.Warn("OneBot bridge log event ACK failed")
		}
	}
}

const onebotBridgeLogDisplayMaxBytes = 16 * 1024

var (
	onebotBridgeLogOpenIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	onebotBridgeLogTinyIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
)

func parseOnebotBridgeLogDisplay(raw json.RawMessage) *onebotBridgeLogDisplay {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(trimmed) > onebotBridgeLogDisplayMaxBytes || trimmed[0] != '{' {
		return nil
	}
	var display onebotBridgeLogDisplay
	if err := json.Unmarshal(trimmed, &display); err != nil || len(display.AuthorAliases) > 8 || len(display.Mentions) > 64 {
		return nil
	}
	for _, alias := range display.AuthorAliases {
		if !validOnebotBridgeLogAlias(alias) {
			return nil
		}
	}
	for mentionIndex := range display.Mentions {
		mention := &display.Mentions[mentionIndex]
		if !validOnebotBridgeLogAlias(mention.Target) || len(mention.Aliases) > 8 ||
			(mention.Name != "" && !validOnebotBridgeLogDisplayName(mention.Name)) {
			return nil
		}
		seen := map[string]struct{}{mention.Target: {}}
		aliases := make([]string, 0, len(mention.Aliases))
		for _, alias := range mention.Aliases {
			if !validOnebotBridgeLogAlias(alias) {
				return nil
			}
			if _, exists := seen[alias]; exists {
				continue
			}
			seen[alias] = struct{}{}
			aliases = append(aliases, alias)
			if len(seen) > 8 {
				return nil
			}
		}
		mention.Aliases = aliases
	}
	return &display
}

func validOnebotBridgeLogAlias(alias string) bool {
	if len(alias) == 0 || len(alias) > 160 || strings.Count(alias, ":") != 1 {
		return false
	}
	parts := strings.SplitN(alias, ":", 2)
	switch parts[0] {
	case "openid":
		return onebotBridgeLogOpenIDPattern.MatchString(parts[1])
	case "tinyid":
		return onebotBridgeLogTinyIDPattern.MatchString(parts[1])
	default:
		return false
	}
}

func validOnebotBridgeLogDisplayName(name string) bool {
	if len(name) == 0 || len(name) > 256 || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 80 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

func validateOnebotBridgeLogEvent(event *onebotBridgeLogEvent) error {
	if event == nil || event.Version != 1 || !validOnebotBridgeEventID(event.EventID) {
		return errors.New("invalid event identity")
	}
	if event.IsBot == nil || event.Time <= 0 || len(event.Nickname) > 1024 || len(event.Text) > onebotBridgeCaptureMaxText ||
		!utf8.ValidString(event.Nickname) || !utf8.ValidString(event.Text) {
		return errors.New("invalid event content")
	}
	if !validOnebotBridgeNumericID(event.GroupID) {
		return errors.New("invalid virtual group id")
	}
	if !validOnebotBridgeNumericID(event.UserID) {
		return errors.New("invalid virtual user id")
	}
	if event.Kind != "message" && event.Kind != "gap" {
		return errors.New("invalid event kind")
	}
	return nil
}

func validOnebotBridgeEventID(eventID string) bool {
	if eventID == "" || len(eventID) > 220 || strings.TrimSpace(eventID) != eventID || !utf8.ValidString(eventID) {
		return false
	}
	for _, r := range eventID {
		if r < 0x20 || r >= 0x7f && r <= 0x9f {
			return false
		}
	}
	return true
}

func validOnebotBridgeConnectionID(connectionID string) bool {
	if connectionID == "" || len(connectionID) > 128 || strings.TrimSpace(connectionID) != connectionID {
		return false
	}
	for _, r := range connectionID {
		if r < 0x20 || r >= 0x7f && r <= 0x9f {
			return false
		}
	}
	return true
}

func parseOnebotBridgeVirtualID(raw string) (int64, bool) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || !validOnebotBridgeNumericID(value) || strconv.FormatInt(value, 10) != raw {
		return 0, false
	}
	return value, true
}

func validOnebotBridgeNumericID(value int64) bool {
	return value >= onebotBridgeMasterIDMin && value < onebotBridgeMasterIDMax
}

func (p *PlatformAdapterOnebot) emitOnebotBridgeLogAck(conn *onebotBridgeConnection, connectionID, eventID, status string) error {
	if conn == nil || conn.emitter == nil || (status != "ok" && status != "failed") || !validOnebotBridgeEventID(eventID) {
		return errors.New("invalid OneBot bridge log ACK")
	}
	currentID, registered := conn.registration()
	if !registered || currentID != connectionID {
		return errors.New("OneBot bridge connection changed")
	}
	ctx := conn.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	actionCtx, cancel := context.WithTimeout(ctx, onebotBridgeActionTimeout)
	defer cancel()
	if err := actionCtx.Err(); err != nil {
		return errOnebotBridgeLogAckActionFailed
	}
	params := onebotBridgeLogAckParams{Version: 1, ConnectionID: connectionID, EventID: eventID, Status: status}
	responseBytes, err := conn.emitter.Raw(actionCtx, onebotLLMBridgeLogAckAction, params)
	if err != nil {
		return errOnebotBridgeLogAckActionFailed
	}
	return validateOnebotBridgeActionResponse(responseBytes)
}

func validateOnebotBridgeActionResponse(responseBytes []byte) error {
	var response onebotActionResponse
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		return errors.New("malformed OneBot bridge action ACK")
	}
	if !strings.EqualFold(response.Status, "ok") || response.RetCode != 0 {
		return errors.New("OneBot bridge action rejected")
	}
	return nil
}

func (p *PlatformAdapterOnebot) appendOnebotBridgeReconnectGaps(conn *onebotBridgeConnection, connectionID string) error {
	if p == nil || conn == nil || p.EndPoint == nil || p.EndPoint.Session == nil || p.EndPoint.Session.Parent == nil {
		return nil
	}
	if !validOnebotBridgeConnectionID(connectionID) {
		return errors.New("invalid connection identity")
	}
	d := p.EndPoint.Session.Parent
	states, err := service.OnebotBridgeLogActiveStates(d.DBOperator)
	if err != nil {
		return err
	}
	for _, state := range states {
		groupNumber, ok := parseOnebotBridgeVirtualID(strings.TrimPrefix(state.GroupID, "QQ-Group:"))
		if !ok || !strings.HasPrefix(state.GroupID, "QQ-Group:") {
			continue
		}
		eventID := fmt.Sprintf("reconnect-gap:%s:%d", connectionID, groupNumber)
		item := &model.LogOneItem{
			Nickname:  "SeaDice记录系统",
			IMUserID:  "system",
			UniformID: "OneBotBridge:system",
			Time:      time.Now().Unix(),
			Message:   "记录缺口：OneBot连接重启或中断期间的群消息可能未被采集，当前日志不保证完整。",
			IsDice:    true,
			CommandInfo: map[string]string{
				"bridgeCaptureKind": "gap",
			},
		}
		if _, _, err := service.LogCaptureAppend(d.DBOperator, eventID, state.GroupID, "gap", item); err != nil {
			return err
		}
	}
	return nil
}

func (r *onebotBridgeRequestTracker) emitArtifact(filename, mediaType string, data []byte) error {
	if r == nil || r.connection == nil || r.connection.emitter == nil || r.audience != "group" || len(data) == 0 || len(data) > onebotBridgeArtifactMaxBytes {
		return errors.New("invalid OneBot bridge artifact")
	}
	ext := filepath.Ext(filename)
	if filepath.Base(filename) != filename || filename == "." || len(filename) > 255 || !utf8.ValidString(filename) ||
		strings.ContainsAny(filename, `/\\`) || ext != ".md" && ext != ".txt" {
		return errors.New("invalid artifact filename")
	}
	if ext == ".md" && mediaType != "text/markdown" || ext == ".txt" && mediaType != "text/plain" || !utf8.Valid(data) {
		return errors.New("invalid artifact content")
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return errors.New("invalid artifact filename")
		}
	}
	connectionID, registered := r.connection.registration()
	if !registered || connectionID != r.connectionID {
		return errors.New("OneBot bridge connection changed")
	}
	r.mu.Lock()
	canSend := !r.completionFired && r.tasks > 0
	r.mu.Unlock()
	if !canSend {
		return errors.New("OneBot bridge request is already complete")
	}
	ctx := r.connection.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	actionCtx, cancel := context.WithTimeout(ctx, onebotBridgeActionTimeout)
	defer cancel()
	if err := actionCtx.Err(); err != nil {
		r.recordSend(false)
		return errOnebotBridgeArtifactActionFailed
	}
	params := onebotBridgeArtifactParams{
		Version:         1,
		ConnectionID:    r.connectionID,
		SourceMessageID: r.sourceMessage,
		Filename:        filename,
		MediaType:       mediaType,
		BytesBase64:     base64.StdEncoding.EncodeToString(data),
	}
	responseBytes, err := r.connection.emitter.Raw(actionCtx, onebotLLMBridgeArtifactAction, params)
	if err != nil {
		err = errOnebotBridgeArtifactActionFailed
	} else {
		err = validateOnebotBridgeActionResponse(responseBytes)
	}
	r.recordSend(err == nil)
	return err
}

func onebotBridgeMarkdownColor(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("#%02x%02x%02x", 70+digest[0]%130, 70+digest[1]%130, 70+digest[2]%130)
}

func onebotBridgeEscapeTitle(text string) string {
	text = html.EscapeString(text)
	var out strings.Builder
	for _, r := range text {
		if strings.ContainsRune("\\`*_{}[]<>()#+-.!|~", r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func onebotBridgeRenderLogSnapshot(operator engine2.DatabaseOperator, groupID, name, format string) ([]byte, error) {
	if format != "md" && format != "txt" {
		return nil, errors.New("unsupported log format")
	}
	if !strings.HasPrefix(groupID, "QQ-Group:") {
		return nil, errors.New("invalid virtual group id")
	}
	if _, ok := parseOnebotBridgeVirtualID(strings.TrimPrefix(groupID, "QQ-Group:")); !ok {
		return nil, errors.New("invalid virtual group id")
	}
	out := &onebotBridgeLimitedBuffer{}
	if err := onebotBridgeWriteLogHeader(out, name, format); err != nil {
		return nil, err
	}
	writer := newOnebotBridgeLogSnapshotWriter(out, format)
	_, err := service.LogCaptureSnapshotWalkWithPrepare(operator, groupID, name, 8, func(page []*model.LogOneItem) error {
		return writer.indexPage(page)
	}, func(page []*model.LogOneItem) error {
		return writer.writePage(page)
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type onebotBridgeLimitedBuffer struct {
	bytes.Buffer
}

func (b *onebotBridgeLimitedBuffer) Write(data []byte) (int, error) {
	if len(data) > onebotBridgeArtifactMaxBytes-b.Len() {
		return 0, errOnebotBridgeArtifactTooLarge
	}
	return b.Buffer.Write(data)
}

func (b *onebotBridgeLimitedBuffer) WriteString(data string) (int, error) {
	if len(data) > onebotBridgeArtifactMaxBytes-b.Len() {
		return 0, errOnebotBridgeArtifactTooLarge
	}
	return b.Buffer.WriteString(data)
}

func onebotBridgeArtifactFilename(logName, format string) string {
	var filenamePart strings.Builder
	characters := 0
	for _, r := range logName {
		var next rune
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			next = r
		case r == ' ':
			next = '-'
		default:
			continue
		}
		if characters >= 80 || filenamePart.Len()+utf8.RuneLen(next) > 160 {
			break
		}
		filenamePart.WriteRune(next)
		characters++
	}
	clean := strings.Trim(filenamePart.String(), "-_")
	if clean == "" {
		clean = "log"
	}
	return fmt.Sprintf("%s.%s", clean, format)
}
