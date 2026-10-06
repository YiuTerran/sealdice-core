package dice

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"sealdice-core/dice/service"
	"sealdice-core/model"
)

type onebotBridgeLogCommand struct {
	action string
	name   string
	format string
}

func onebotBridgeParseLogCommand(raw string) *onebotBridgeLogCommand {
	if utf8.RuneCountInString(raw) > 4000 || !utf8.ValidString(raw) || strings.TrimSpace(raw) != raw ||
		!strings.HasPrefix(raw, ".log ") || strings.ContainsAny(raw, "\r\n\t\x00") {
		return nil
	}
	body := strings.TrimPrefix(raw, ".log ")
	if body == "" || strings.HasPrefix(body, " ") || strings.HasSuffix(body, " ") {
		return nil
	}
	verb, rest, hasRest := strings.Cut(body, " ")
	if hasRest && (rest == "" || strings.HasPrefix(rest, " ")) {
		return nil
	}
	cmd := &onebotBridgeLogCommand{action: verb, format: "md"}
	if !hasRest {
		rest = ""
	}
	switch verb {
	case "new", "on", "stat", "del":
		cmd.name = rest
	case "get", "export":
		if rest == "--format=txt" {
			cmd.format = "txt"
			rest = ""
		} else if strings.HasSuffix(rest, " --format=txt") {
			cmd.format = "txt"
			rest = strings.TrimSuffix(rest, " --format=txt")
		} else if strings.Contains(rest, "--format=") {
			return nil
		}
		cmd.name = rest
	case "off", "halt", "end", "list":
		if hasRest {
			return nil
		}
	default:
		return nil
	}
	if cmd.name != "" {
		if utf8.RuneCountInString(cmd.name) > 80 || strings.TrimSpace(cmd.name) != cmd.name || !utf8.ValidString(cmd.name) ||
			strings.ContainsAny(cmd.name, "\x00\r\n@") || strings.Contains(cmd.name, " ") || strings.HasPrefix(cmd.name, "--") ||
			strings.Contains(strings.ToLower(cmd.name), "cq:") {
			return nil
		}
		for _, r := range cmd.name {
			if unicode.IsControl(r) || unicode.IsSpace(r) {
				return nil
			}
		}
	}
	switch verb {
	case "new", "on", "stat", "get", "export":
		// An omitted name means the current log, except for new which generates
		// the same timestamp-style name as SeaDice's native command.
	case "del":
		if cmd.name == "" {
			return nil
		}
	case "off", "halt", "end", "list":
	}
	return cmd
}

func onebotBridgeLogActionRequiresAdmin(action string) bool {
	switch action {
	case "new", "on", "off", "halt", "end", "del":
		return true
	default:
		return false
	}
}

func executeOnebotBridgeLogCommand(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) {
	if ctx == nil || ctx.Dice == nil || ctx.Session == nil || msg == nil || cmdArgs == nil ||
		ctx.LLMBridgeRequest == nil || ctx.IsPrivate || ctx.MessageType != "group" {
		if ctx != nil && ctx.LLMBridgeRequest != nil {
			ctx.LLMBridgeRequest.markFailedAt("log_group_required")
		}
		return
	}
	tracker := ctx.LLMBridgeRequest
	connectionID, registered := tracker.connection.registration()
	if !registered || connectionID != tracker.connectionID || tracker.groupID <= 0 ||
		ctx.Group == nil || ctx.Group.GroupID != fmt.Sprintf("QQ-Group:%d", tracker.groupID) {
		tracker.markFailedAt("log_group_mismatch")
		return
	}
	parsed := onebotBridgeParseLogCommand(cmdArgs.RawText)
	if parsed == nil || msg.Message != cmdArgs.RawText {
		tracker.markFailedAt("log_command_shape_rejected")
		return
	}
	if onebotBridgeLogActionRequiresAdmin(parsed.action) {
		if allowed, stage := tracker.groupRoleAuthorization(); !allowed {
			tracker.markFailedAt(stage)
			return
		}
	}
	groupID := ctx.Group.GroupID
	state, err := service.OnebotBridgeLogStateGet(ctx.Dice.DBOperator, groupID)
	if errors.Is(err, service.ErrBridgeLogStateNotFound) {
		state = model.OnebotBridgeLogState{GroupID: groupID}
		err = nil
	}
	if err != nil {
		onebotBridgeLogReply(ctx, msg, false, "日志状态读取失败")
		return
	}

	switch parsed.action {
	case "new":
		name := parsed.name
		if name == "" {
			if state.Name != "" {
				onebotBridgeLogReply(ctx, msg, false, "当前日志尚未结束，请先使用 .log end 或 .log halt")
				return
			}
			name = time.Now().Format("2006_01_02_15_04_05")
		}
		state, err = service.OnebotBridgeLogNew(ctx.Dice.DBOperator, groupID, name)
		if err == nil {
			err = onebotBridgeMirrorLogState(ctx, state)
		}
		if err != nil {
			onebotBridgeLogReply(ctx, msg, false, "日志新建失败")
			return
		}
		onebotBridgeLogReply(ctx, msg, true, "已新建并开启日志："+name)
	case "on":
		if state.On {
			onebotBridgeLogReply(ctx, msg, false, "当前日志已经开启")
			return
		}
		name := parsed.name
		if name == "" {
			name = state.Name
		}
		if name == "" {
			onebotBridgeLogReply(ctx, msg, false, "请指定已存在的日志名，或先使用 .log new")
			return
		}
		if !onebotBridgeLogExists(ctx, groupID, name) {
			onebotBridgeLogReply(ctx, msg, false, "找不到当前群的这份日志")
			return
		}
		var logID uint64
		logID, err = service.LogGetOrCreate(ctx.Dice.DBOperator, groupID, name)
		if err == nil {
			state = model.OnebotBridgeLogState{GroupID: groupID, LogID: logID, Name: name, On: true}
			err = service.OnebotBridgeLogStateSave(ctx.Dice.DBOperator, state)
		}
		if err == nil {
			err = onebotBridgeMirrorLogState(ctx, state)
		}
		if err != nil {
			onebotBridgeLogReply(ctx, msg, false, "日志开启失败")
			return
		}
		onebotBridgeLogReply(ctx, msg, true, "已开启日志："+name)
	case "off":
		if state.Name == "" || !state.On {
			onebotBridgeLogReply(ctx, msg, false, "当前没有正在记录的日志")
			return
		}
		state.On = false
		if err = service.OnebotBridgeLogStateSave(ctx.Dice.DBOperator, state); err == nil {
			err = onebotBridgeMirrorLogState(ctx, state)
		}
		if err != nil {
			onebotBridgeLogReply(ctx, msg, false, "日志暂停失败")
			return
		}
		onebotBridgeLogReply(ctx, msg, true, "已暂停日志："+state.Name)
	case "halt":
		state = model.OnebotBridgeLogState{GroupID: groupID}
		if err = service.OnebotBridgeLogStateSave(ctx.Dice.DBOperator, state); err == nil {
			err = onebotBridgeMirrorLogState(ctx, state)
		}
		if err != nil {
			onebotBridgeLogReply(ctx, msg, false, "日志终止失败")
			return
		}
		onebotBridgeLogReply(ctx, msg, true, "已终止当前日志")
	case "end":
		if state.Name == "" || state.LogID == 0 {
			onebotBridgeLogReply(ctx, msg, false, "当前没有可结束的日志")
			return
		}
		if !onebotBridgeEmitLogArtifact(ctx, msg, state, parsed.format) {
			return
		}
		state = model.OnebotBridgeLogState{GroupID: groupID}
		if err = service.OnebotBridgeLogStateSave(ctx.Dice.DBOperator, state); err == nil {
			err = onebotBridgeMirrorLogState(ctx, state)
		}
		if err != nil {
			tracker.markFailedAt("log_end_state_save_failed")
			return
		}
		tracker.markCommandResult(true)
	case "list":
		logs, listErr := service.LogGetList(ctx.Dice.DBOperator, groupID)
		if listErr != nil {
			onebotBridgeLogReply(ctx, msg, false, "日志列表读取失败")
			return
		}
		if len(logs) == 0 {
			onebotBridgeLogReply(ctx, msg, true, "当前群没有日志")
			return
		}
		var lines strings.Builder
		for _, name := range logs {
			label := ""
			if name == state.Name {
				if state.On {
					label = "（记录中）"
				} else {
					label = "（已暂停）"
				}
			}
			fmt.Fprintf(&lines, "- %s%s\n", name, label)
		}
		onebotBridgeLogReply(ctx, msg, true, "当前群日志：\n"+strings.TrimRight(lines.String(), "\n"))
	case "stat":
		name := parsed.name
		if name == "" {
			name = state.Name
		}
		if name == "" {
			onebotBridgeLogReply(ctx, msg, false, "请指定日志名，或先使用 .log new")
			return
		}
		lines, exists := service.LogLinesCountGet(ctx.Dice.DBOperator, groupID, name)
		if !exists {
			onebotBridgeLogReply(ctx, msg, false, "找不到当前群的这份日志")
			return
		}
		var gaps int64
		gaps, err = service.LogCaptureGapCountByName(ctx.Dice.DBOperator, groupID, name)
		if err != nil {
			onebotBridgeLogReply(ctx, msg, false, "日志缺口统计读取失败")
			return
		}
		status := "已暂停"
		if state.On && name == state.Name {
			status = "记录中"
		}
		onebotBridgeLogReply(ctx, msg, true, fmt.Sprintf("日志：%s\n状态：%s\n条目：%d\n已记录缺口：%d", name, status, lines, gaps))
	case "get", "export":
		name := parsed.name
		if name == "" {
			name = state.Name
		}
		if name == "" {
			onebotBridgeLogReply(ctx, msg, false, "请指定日志名，或先使用 .log new")
			return
		}
		target := state
		if target.Name != name || target.LogID == 0 {
			logs, listErr := service.LogGetList(ctx.Dice.DBOperator, groupID)
			if listErr != nil || !stringSliceContains(logs, name) {
				onebotBridgeLogReply(ctx, msg, false, "找不到当前群的这份日志")
				return
			}
			id, idErr := service.LogGetOrCreate(ctx.Dice.DBOperator, groupID, name)
			if idErr != nil {
				onebotBridgeLogReply(ctx, msg, false, "日志读取失败")
				return
			}
			target = model.OnebotBridgeLogState{GroupID: groupID, LogID: id, Name: name}
		}
		if !onebotBridgeEmitLogArtifact(ctx, msg, target, parsed.format) {
			return
		}
		tracker.markCommandResult(true)
	case "del":
		if parsed.name == state.Name {
			onebotBridgeLogReply(ctx, msg, false, "不能删除当前选中的日志，请先使用 .log halt")
			return
		}
		if !onebotBridgeLogExists(ctx, groupID, parsed.name) {
			onebotBridgeLogReply(ctx, msg, false, "找不到当前群的这份日志")
			return
		}
		if err = service.LogDelete(ctx.Dice.DBOperator, groupID, parsed.name); err != nil {
			onebotBridgeLogReply(ctx, msg, false, "日志删除失败")
			return
		}
		onebotBridgeLogReply(ctx, msg, true, "已删除日志："+parsed.name)
	}
}

func onebotBridgeLogReply(ctx *MsgContext, msg *Message, solved bool, text string) {
	ReplyToSender(ctx, msg, text)
	ctx.LLMBridgeRequest.markCommandResult(solved)
}

func onebotBridgeMirrorLogState(ctx *MsgContext, state model.OnebotBridgeLogState) error {
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil || state.GroupID != ctx.Group.GroupID {
		return errors.New("bridge log group is unavailable")
	}
	ctx.Group.SetLogState(state.LogID, state.Name, state.On)
	ctx.Group.MarkDirty(ctx.Dice)
	encoded, err := json.Marshal(ctx.Group)
	if err != nil {
		return err
	}
	return service.GroupInfoSave(ctx.Dice.DBOperator, ctx.Group.GroupID, ctx.Group.UpdatedAtTime, encoded)
}

func onebotBridgeLogExists(ctx *MsgContext, groupID, name string) bool {
	logs, err := service.LogGetList(ctx.Dice.DBOperator, groupID)
	return err == nil && stringSliceContains(logs, name)
}

func stringSliceContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func onebotBridgeEmitLogArtifact(ctx *MsgContext, msg *Message, state model.OnebotBridgeLogState, format string) bool {
	if ctx == nil || ctx.Dice == nil || ctx.LLMBridgeRequest == nil || state.Name == "" || state.LogID == 0 {
		return false
	}
	groupNumber, ok := parseOnebotBridgeVirtualID(strings.TrimPrefix(state.GroupID, "QQ-Group:"))
	if !strings.HasPrefix(state.GroupID, "QQ-Group:") || !ok {
		ctx.LLMBridgeRequest.markFailedAt("log_group_id_invalid")
		return false
	}
	data, err := onebotBridgeRenderLogSnapshot(ctx.Dice.DBOperator, state.GroupID, state.Name, format)
	if err != nil {
		onebotBridgeLogReply(ctx, msg, false, "日志导出失败：超出10 MiB限制或快照读取失败")
		return false
	}
	ext := format
	mediaType := "text/markdown"
	if format == "txt" {
		mediaType = "text/plain"
	}
	filename := onebotBridgeArtifactFilename(groupNumber, state.Name, ext)
	if err := ctx.LLMBridgeRequest.emitArtifact(filename, mediaType, data); err != nil {
		ctx.LLMBridgeRequest.markFailedAt("log_artifact_rejected")
		return false
	}
	return true
}
