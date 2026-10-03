package dice

import (
	"fmt"
	"strings"
)

var onebotBridgeNativeCommands = map[string]struct{}{
	"r": {}, "rh": {}, "ra": {}, "rc": {}, "st": {}, "pc": {}, "sc": {}, "en": {}, "set": {},
}

// executeOnebotBridgeNew keeps the regular native parser and PreTriggerCommand
// lifecycle while bypassing all free-form extension hooks and custom replies.
func (s *IMSession) executeOnebotBridgeNew(ep *EndPointInfo, msg *Message) {
	tracker := msg.LLMBridgeRequest
	if tracker == nil || ep == nil || s == nil || s.Parent == nil {
		if tracker != nil {
			tracker.markFailedAt("missing_execution_context")
		}
		return
	}
	d := s.Parent
	mctx := &MsgContext{
		Dice:                d,
		MessageType:         msg.MessageType,
		IsPrivate:           msg.MessageType == "private",
		Session:             s,
		EndPoint:            ep,
		UITestReplySplitLen: msg.UITestReplySplitLen,
		LLMBridgeRequest:    tracker,
	}
	if msg.MessageType != "group" && msg.MessageType != "private" {
		tracker.markFailedAt("unsupported_message_type")
		return
	}
	if msg.MessageType == "group" {
		groupInfo, ok := s.ServiceAtNew.Load(msg.GroupID)
		if !ok && msg.GroupID != "" {
			groupInfo = SetBotOnAtGroup(mctx, msg.GroupID)
			groupInfo.Active = true
			groupInfo.DiceIDExistsMap.Store(ep.UserID, true)
			if msg.GroupName != "" {
				groupInfo.GroupName = msg.GroupName
			}
			groupInfo.MarkDirty(d)
		}
		if groupInfo != nil && msg.GroupName != "" {
			groupInfo.GroupName = msg.GroupName
		}
	}
	mctx.Group, mctx.Player = GetPlayerInfoBySender(mctx, msg)
	if mctx.Player == nil {
		tracker.markFailedAt("missing_player")
		return
	}
	if mctx.Group != nil && mctx.Group.System != "" {
		mctx.SystemTemplate = mctx.Group.GetCharTemplate(d)
	}
	// A virtual sender is always treated as an ordinary player, regardless of
	// role text supplied in the OneBot event or saved trust/master configuration.
	msg.Sender.GroupRole = ""
	_ = mctx.fillPrivilege(msg)
	if mctx.PrivilegeLevel != -30 {
		mctx.PrivilegeLevel = 0
	}
	mctx.GroupRoleLevel = 0
	VarSetValueStr(mctx, "$tMsgID", fmt.Sprintf("%v", msg.RawID))
	mctx.IsCurGroupBotOn = msg.MessageType == "group" && mctx.Group != nil && mctx.Group.IsActive(mctx)

	cmdArgs := CommandParseNew(mctx, msg)
	if cmdArgs == nil {
		tracker.markFailedAt("command_parse_failed")
		return
	}
	command := strings.ToLower(cmdArgs.Command)
	if _, allowed := onebotBridgeNativeCommands[command]; !allowed {
		tracker.markFailedAt("command_not_allowed")
		return
	}
	if command == "set" && !isOnebotBridgeRuleSelection(mctx, cmdArgs) {
		tracker.markFailedAt("set_not_rule_selection")
		return
	}
	banMessage := *msg
	banMessage.Message = ""
	if checkBan(mctx, &banMessage) || mctx.PrivilegeLevel == -30 {
		tracker.markFailedAt("banned_or_denied")
		return
	}
	mctx.CommandID = getNextCommandID()
	SetTempVars(mctx, msg.Sender.Nickname)

	if !tracker.beginTask() {
		tracker.markFailedAt("request_already_terminal")
		return
	}
	go func() {
		defer tracker.finishTask()
		defer func() {
			if recovered := recover(); recovered != nil {
				tracker.markFailedAt("native_command_panic")
				d.Logger.Warnf("OneBot LLM bridge native command failed: source_message_id=%d", tracker.sourceMessage)
			}
		}()
		s.PreTriggerCommand(mctx, msg, cmdArgs)
	}()
}

func isOnebotBridgeRuleSelection(ctx *MsgContext, cmdArgs *CmdArgs) bool {
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil || ctx.IsPrivate || len(cmdArgs.Args) != 1 || len(cmdArgs.Kwargs) != 0 {
		return false
	}
	key := cmdArgs.Args[0]
	matched := false
	ctx.Dice.GameSystemMap.Range(func(_ string, tmpl *GameSystemTemplate) bool {
		if tmpl == nil {
			return true
		}
		for _, candidate := range tmpl.SetConfig.Keys {
			if strings.EqualFold(candidate, key) {
				matched = true
				return false
			}
		}
		return true
	})
	return matched
}
