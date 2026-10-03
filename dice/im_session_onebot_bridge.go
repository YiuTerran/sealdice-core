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
			tracker.markFailed()
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
		tracker.markFailed()
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
		tracker.markFailed()
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
		tracker.markFailed()
		return
	}
	command := strings.ToLower(cmdArgs.Command)
	if _, allowed := onebotBridgeNativeCommands[command]; !allowed {
		tracker.markFailed()
		return
	}
	if command == "set" && !isOnebotBridgeRuleSelection(mctx, cmdArgs) {
		tracker.markFailed()
		return
	}
	banMessage := *msg
	banMessage.Message = ""
	if checkBan(mctx, &banMessage) || mctx.PrivilegeLevel == -30 {
		tracker.markFailed()
		return
	}
	mctx.CommandID = getNextCommandID()
	SetTempVars(mctx, msg.Sender.Nickname)

	if !tracker.beginTask() {
		tracker.markFailed()
		return
	}
	go func() {
		defer tracker.finishTask()
		defer func() {
			if recovered := recover(); recovered != nil {
				tracker.markFailed()
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
