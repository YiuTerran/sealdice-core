package dice

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"sealdice-core/dice/service"
	"sealdice-core/message"
)

func verifyOnebotBridgeBackup(filename string, diceDataDir string) error {
	if filename == "" {
		return errors.New("invalid backup filename")
	}
	cleanName := filepath.Clean(filename)
	if filepath.Dir(cleanName) != "." && filepath.Dir(cleanName) != filepath.Clean(BackupDir) {
		return errors.New("invalid backup location")
	}
	filename = filepath.Base(cleanName)
	file, err := os.Open(filepath.Join(BackupDir, filename))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	archive, err := zip.NewReader(file, info.Size())
	if err != nil {
		return err
	}
	if len(archive.File) == 0 {
		return errors.New("backup archive is empty")
	}
	required := map[string]bool{
		"backup_info.json": false,
		filepath.ToSlash(filepath.Clean(filepath.Join(diceDataDir, "data.db"))): false,
	}
	for _, entry := range archive.File {
		if _, ok := required[entry.Name]; ok {
			required[entry.Name] = true
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(io.Discard, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for name, found := range required {
		if !found {
			return fmt.Errorf("backup archive is missing %s", name)
		}
	}
	return nil
}

var onebotBridgeNativeCommands = map[string]struct{}{
	// Keep this list explicit: several native command aliases trigger hidden
	// rolls, delegated rolls, or stateful helpers that are not part of the bridge.
	"r": {}, "rd": {}, "roll": {}, "ra": {}, "rc": {}, "st": {}, "pc": {}, "sc": {}, "en": {}, "set": {},
	"ww": {}, "dx": {}, "ek": {}, "rsr": {}, "coc": {}, "dnd": {}, "dndx": {}, "ti": {}, "li": {},
	"userid": {}, "find": {}, "查询": {}, "setcoc": {}, "ss": {}, "buff": {}, "ds": {}, "死亡豁免": {}, "init": {},
	"jrrp": {}, "gugu": {}, "咕咕": {}, "ping": {}, "master": {}, "ban": {},
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
	bridgeInput := extractResultFromSegments(msg.Segment)
	if !onebotBridgeInputAllowed(bridgeInput) {
		tracker.markFailedAt("command_too_long")
		return
	}
	if msg.MessageType == "group" {
		// Parse against a detached view before group activation, extension sync,
		// or player registration can change shared state for a denied rule write.
		preview := &MsgContext{Dice: d, Session: s, EndPoint: ep, MessageType: "group",
			Group: onebotBridgePreviewGroup(mctx, msg.GroupID)}
		previewArgs := (&CmdArgs{}).commandParseNew(preview, msg, true)
		if previewArgs != nil && onebotBridgeCommandRequiresGroupAdmin(preview, strings.ToLower(previewArgs.Command), previewArgs) {
			if allowed, failureStage := tracker.groupRoleAuthorization(); !allowed {
				tracker.markFailedAt(failureStage)
				return
			}
		}
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
	if msg.MessageType == "private" {
		mctx.Group, mctx.Player = onebotBridgePrivateQueryContext(mctx, msg)
	} else {
		mctx.Group, mctx.Player = GetPlayerInfoBySender(mctx, msg)
	}
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

	cmdArgs := (&CmdArgs{}).commandParseNew(mctx, msg, true)
	if cmdArgs == nil {
		tracker.markFailedAt("command_parse_failed")
		return
	}
	command := strings.ToLower(cmdArgs.Command)
	if !onebotBridgeCommandAllowed(mctx, command, cmdArgs) {
		tracker.markFailedAt("command_not_allowed")
		return
	}
	if onebotBridgeCommandRequiresGroupAdmin(mctx, command, cmdArgs) {
		if allowed, failureStage := tracker.groupRoleAuthorization(); !allowed {
			tracker.markFailedAt(failureStage)
			return
		}
	}
	banMessage := *msg
	banMessage.Message = ""
	if checkBan(mctx, &banMessage) || mctx.PrivilegeLevel == -30 {
		tracker.markFailedAt("banned_or_denied")
		return
	}
	if command == "master" || command == "ban" {
		if !isOnebotBridgeMasterCommand(mctx, msg, cmdArgs) {
			tracker.markFailedAt("master_acl_denied")
			return
		}
		mctx.PrivilegeLevel = 100
	}
	mctx.LLMBridgeReadOnly = onebotBridgeIsReadOnlyCommand(command, cmdArgs)
	if msg.MessageType == "private" && !mctx.LLMBridgeReadOnly {
		mctx.Group, mctx.Player = GetPlayerInfoBySender(mctx, msg)
		if mctx.Group != nil && mctx.Group.System != "" {
			mctx.SystemTemplate = mctx.Group.GetCharTemplate(d)
		}
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

// onebotBridgePrivateQueryContext gives private read-only commands a transient
// private group/player view. The normal GetPlayerInfoBySender path activates
// and marks PG-* groups dirty even for queries.
func onebotBridgePreviewGroup(ctx *MsgContext, groupID string) *GroupInfo {
	source, _ := ctx.Session.ServiceAtNew.Load(groupID)
	group := &GroupInfo{
		GroupID:           groupID,
		Active:            true,
		CocRuleIndex:      int(ctx.Dice.Config.DefaultCocRuleIndex),
		Players:           new(SyncMap[string, *GroupPlayerInfo]),
		InactivatedExtSet: StringSet{},
		ExtAppliedTime:    1,
	}
	if source != nil {
		group.GroupName = source.GroupName
		group.System = source.System
		group.DiceSideNum = source.DiceSideNum
		group.CocRuleIndex = source.CocRuleIndex
		group.DefaultHelpGroup = source.DefaultHelpGroup
		group.ShowGroupWelcome = source.ShowGroupWelcome
		group.GroupWelcomeMessage = source.GroupWelcomeMessage
		source.extInitMu.Lock()
		for key := range source.InactivatedExtSet {
			group.InactivatedExtSet[key] = struct{}{}
		}
		for _, existing := range source.activatedExtList {
			if existing == nil {
				continue
			}
			if live := ctx.Dice.ExtFind(existing.Name, false); live != nil {
				group.activatedExtList = append(group.activatedExtList, live)
			} else {
				group.activatedExtList = append(group.activatedExtList, existing)
			}
		}
		source.extInitMu.Unlock()
		logState := source.GetLogState()
		group.SetLogState(logState.ID, logState.Name, logState.On)
	}
	activated := make(map[string]struct{}, len(group.activatedExtList))
	for _, ext := range group.activatedExtList {
		if ext != nil {
			activated[ext.Name] = struct{}{}
		}
	}
	for _, setting := range ctx.Dice.Config.ExtDefaultSettings {
		if setting == nil || !setting.AutoActive || setting.ExtItem == nil || group.IsExtInactivated(setting.Name) {
			continue
		}
		if _, exists := activated[setting.Name]; exists {
			continue
		}
		group.activatedExtList = append(group.activatedExtList, setting.ExtItem)
		activated[setting.Name] = struct{}{}
	}
	return group
}

func onebotBridgePrivateQueryContext(ctx *MsgContext, msg *Message) (*GroupInfo, *GroupPlayerInfo) {
	groupID := "PG-" + msg.Sender.UserID
	group := onebotBridgePreviewGroup(ctx, groupID)
	if ctx.Dice.DBOperator != nil {
		if err := service.GroupPlayerIdentityRegister(ctx.Dice.DBOperator, groupID, msg.Sender.UserID, msg.Sender.Nickname); err != nil {
			ctx.Dice.Logger.Warnf("OneBot bridge private identity registration failed: %v", err)
		}
	}
	player := group.PlayerGet(ctx.Dice.DBOperator, msg.Sender.UserID)
	if player == nil {
		player = &GroupPlayerInfo{Name: msg.Sender.Nickname, UserID: msg.Sender.UserID}
	}
	return group, player
}

// onebotBridgeCommandAllowed validates the command name after SeaDice's native
// parser has resolved it. This intentionally rejects undeclared aliases even
// when they point to the same native command implementation.
func onebotBridgeCommandAllowed(ctx *MsgContext, command string, cmdArgs *CmdArgs) bool {
	if cmdArgs == nil {
		return false
	}
	if _, ok := onebotBridgeNativeCommands[command]; !ok {
		return false
	}
	if cmdArgs.SpecialExecuteTimes < 0 || cmdArgs.SpecialExecuteTimes > 10 {
		return false
	}
	if cmdArgs.SpecialExecuteTimes == 0 {
		cleaned, _ := SpecialExecuteTimesParse(cmdArgs.RawText)
		if cleaned != cmdArgs.RawText {
			return false
		}
	}
	if len(cmdArgs.At) != 0 {
		return false
	}
	if (command == "coc" || command == "dnd" || command == "dndx") && !onebotBridgeCardCountAllowed(cmdArgs) {
		return false
	}
	switch command {
	case "set":
		if len(cmdArgs.Kwargs) != 0 {
			return false
		}
		return len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "info") || isOnebotBridgeRuleSelection(ctx, cmdArgs)
	case "ww":
		return len(cmdArgs.Kwargs) == 0 && !cmdArgs.IsArgEqual(1, "set")
	case "userid", "ping", "jrrp", "ss", "buff":
		return len(cmdArgs.Args) == 0 && len(cmdArgs.Kwargs) == 0
	case "ti", "li":
		return len(cmdArgs.Args) == 0 && len(cmdArgs.Kwargs) == 0
	case "setcoc":
		return len(cmdArgs.Kwargs) == 0 && (len(cmdArgs.Args) == 0 || len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "details"))
	case "find", "查询":
		hasRandom := false
		for _, kw := range cmdArgs.Kwargs {
			if kw.Name == "rand" && !kw.ValueExists {
				hasRandom = true
			}
		}
		if (len(cmdArgs.Args) == 0 && !hasRandom) || len(cmdArgs.Args) > 0 && (strings.EqualFold(cmdArgs.Args[0], "config") || strings.EqualFold(cmdArgs.Args[0], "help")) {
			return false
		}
		for _, kw := range cmdArgs.Kwargs {
			if kw.Name == "rand" && !kw.ValueExists {
				continue
			}
			if kw.Name != "num" && kw.Name != "page" || !onebotBridgeFindNumberArg(kw) {
				return false
			}
		}
		return true
	case "ds", "死亡豁免":
		return len(cmdArgs.Kwargs) == 0 && len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "stat")
	case "init":
		return len(cmdArgs.Kwargs) == 0 && (len(cmdArgs.Args) == 0 || len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "list"))
	case "gugu", "咕咕":
		if len(cmdArgs.Kwargs) != 0 || len(cmdArgs.Args) > 1 {
			return false
		}
		if len(cmdArgs.Args) == 0 {
			return true
		}
		arg := strings.ToLower(cmdArgs.Args[0])
		return arg == "from" || arg == "showfrom" || cmdArgs.Args[0] == "来源" || cmdArgs.Args[0] == "作者"
	case "rsr":
		return len(cmdArgs.Kwargs) == 0 && len(cmdArgs.Args) == 1
	case "master", "ban":
		return bridgeAdminCommandShape(command, cmdArgs)
	default:
		// Roll, rule checks, and COC/DND random generation retain their native
		// argument parser but do not accept bridge-level keyword switches.
		return len(cmdArgs.Kwargs) == 0
	}
}

// onebotBridgeCommandRequiresGroupAdmin classifies the only currently exposed
// native operation that changes shared group rules. Personal character and
// attribute commands remain ordinary member operations.
func onebotBridgeCommandRequiresGroupAdmin(ctx *MsgContext, command string, cmdArgs *CmdArgs) bool {
	return command == "set" && isOnebotBridgeRuleSelection(ctx, cmdArgs)
}

func onebotBridgeCardCountAllowed(cmdArgs *CmdArgs) bool {
	if cmdArgs == nil || len(cmdArgs.Kwargs) != 0 || len(cmdArgs.Args) > 1 {
		return false
	}
	if len(cmdArgs.Args) == 0 {
		return true
	}
	var count int
	if _, err := fmt.Sscanf(cmdArgs.Args[0], "%d", &count); err != nil || count < 1 || count > 10 {
		return false
	}
	return strconv.Itoa(count) == cmdArgs.Args[0]
}

func onebotBridgeInputAllowed(text string) bool {
	if utf8.RuneCountInString(text) > 4000 || strings.ContainsAny(text, "\r\n\u2028\u2029") {
		return false
	}
	for _, r := range text {
		if r < 0x20 && r != '\t' || r >= 0x7f && r <= 0x9f {
			return false
		}
	}
	for i, r := range text {
		if r == '@' && i+1 < len(text) {
			next, _ := utf8.DecodeRuneInString(text[i+1:])
			if !unicode.IsSpace(next) {
				return false
			}
		}
	}
	return true
}

func onebotBridgeFindNumberArg(kw *Kwarg) bool {
	if kw == nil || !kw.ValueExists {
		return false
	}
	value, err := strconv.Atoi(kw.Value)
	if err != nil {
		return false
	}
	if kw.Name == "num" {
		return value >= 1 && value <= 10 && strconv.Itoa(value) == kw.Value
	}
	return kw.Name == "page" && value >= 1 && value <= 9999 && strconv.Itoa(value) == kw.Value
}

func bridgeAdminCommandShape(command string, cmdArgs *CmdArgs) bool {
	if cmdArgs == nil || len(cmdArgs.Kwargs) != 0 || !cmdArgs.IsSpaceBeforeArgs {
		return false
	}
	args := cmdArgs.Args
	if command == "master" {
		return len(args) == 1 && (strings.EqualFold(args[0], "list") || strings.EqualFold(args[0], "backup"))
	}
	if len(args) < 1 {
		return false
	}
	switch strings.ToLower(args[0]) {
	case "list":
		return len(args) == 1 || len(args) == 2 && (args[1] == "ban" || args[1] == "warn" || args[1] == "trust")
	case "query", "rm", "trust":
		return len(args) == 2
	case "add":
		return len(args) == 2 || len(args) == 3
	default:
		return false
	}
}

func onebotBridgeMasterCommandAllowed(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) bool {
	if ctx == nil || msg == nil || cmdArgs == nil || ctx.LLMBridgeRequest == nil || !ctx.IsPrivate || msg.MessageType != "private" {
		return false
	}
	tracker := ctx.LLMBridgeRequest
	if tracker.connection == nil || !tracker.connection.hasMasterUser(tracker.connectionID, tracker.userID) {
		return false
	}
	if !bridgeAdminCommandShape(strings.ToLower(cmdArgs.Command), cmdArgs) || cmdArgs.SpecialExecuteTimes != 0 {
		return false
	}
	if strings.ContainsAny(msg.Message, "\r\n") || strings.TrimSpace(msg.Message) != strings.TrimSpace(cmdArgs.RawText) {
		return false
	}
	if len(msg.Segment) == 0 {
		return false
	}
	var text strings.Builder
	for _, segment := range msg.Segment {
		if segment == nil || segment.Type() != message.Text {
			return false
		}
		item, ok := segment.(*message.TextElement)
		if !ok {
			return false
		}
		text.WriteString(item.Content)
	}
	return text.String() == msg.Message
}

func isOnebotBridgeMasterCommand(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) bool {
	if !onebotBridgeMasterCommandAllowed(ctx, msg, cmdArgs) {
		return false
	}
	if strings.EqualFold(cmdArgs.Command, "ban") {
		return onebotBridgeBanTargetKnown(ctx, cmdArgs)
	}
	return true
}

func onebotBridgeBanTargetKnown(ctx *MsgContext, cmdArgs *CmdArgs) bool {
	if ctx == nil || cmdArgs == nil || len(cmdArgs.Args) < 1 {
		return false
	}
	verb := strings.ToLower(cmdArgs.Args[0])
	if verb == "list" {
		return true
	}
	if len(cmdArgs.Args) < 2 {
		return false
	}
	target := cmdArgs.Args[1]
	if strings.HasPrefix(target, "QQ-Group:") {
		if !bridgeNumericID(target, "QQ-Group:") || ctx.Session == nil || ctx.Session.ServiceAtNew == nil {
			return false
		}
		_, ok := ctx.Session.ServiceAtNew.Load(target)
		return ok
	}
	if !bridgeNumericID(target, "QQ:") || ctx.Session == nil || ctx.Session.ServiceAtNew == nil {
		return false
	}
	if ctx.Dice != nil && ctx.Dice.Config.BanList != nil {
		if _, exists := ctx.Dice.Config.BanList.GetByID(target); exists {
			return true
		}
	}
	if ctx.Player != nil && ctx.Player.UserID == target {
		return true
	}
	if ctx.Dice != nil && ctx.Dice.DBOperator != nil && service.GroupPlayerInfoGet(ctx.Dice.DBOperator, "PG-"+target, target) != nil {
		return true
	}
	known := false
	ctx.Session.ServiceAtNew.Range(func(_ string, group *GroupInfo) bool {
		if group == nil {
			return true
		}
		if group.Players != nil {
			if _, ok := group.Players.Load(target); ok {
				known = true
				return false
			}
		}
		if ctx.Dice != nil && ctx.Dice.DBOperator != nil && service.GroupPlayerInfoGet(ctx.Dice.DBOperator, group.GroupID, target) != nil {
			known = true
			return false
		}
		return !known
	})
	return known
}

func bridgeNumericID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	if len(value) == len(prefix) {
		return false
	}
	for _, r := range value[len(prefix):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	parsed, err := strconv.ParseInt(value[len(prefix):], 10, 64)
	return err == nil && parsed >= onebotBridgeMasterIDMin && parsed < onebotBridgeMasterIDMax
}

// onebotBridgeIsReadOnlyCommand identifies query forms whose native Solvers may
// otherwise write defaults or refresh timestamps while serving the response.
func onebotBridgeIsReadOnlyCommand(command string, cmdArgs *CmdArgs) bool {
	if cmdArgs == nil {
		return false
	}
	if command == "find" || command == "查询" {
		return true
	}
	if len(cmdArgs.Kwargs) != 0 {
		return false
	}
	switch command {
	case "userid", "ss", "buff", "ping", "jrrp":
		return len(cmdArgs.Args) == 0
	case "gugu", "咕咕":
		return len(cmdArgs.Args) == 0 || len(cmdArgs.Args) == 1 && (cmdArgs.Args[0] == "来源" || cmdArgs.Args[0] == "作者" || strings.EqualFold(cmdArgs.Args[0], "from") || strings.EqualFold(cmdArgs.Args[0], "showfrom"))
	case "master":
		return len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "list")
	case "set":
		return len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "info")
	case "setcoc":
		return len(cmdArgs.Args) == 0 || len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "details")
	case "ds", "死亡豁免":
		return len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "stat")
	case "init":
		return len(cmdArgs.Args) == 0 || len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "list")
	case "ban":
		return len(cmdArgs.Args) == 1 && strings.EqualFold(cmdArgs.Args[0], "list") ||
			len(cmdArgs.Args) == 2 && (strings.EqualFold(cmdArgs.Args[0], "list") || strings.EqualFold(cmdArgs.Args[0], "query"))
	default:
		return false
	}
}

func isOnebotBridgeRuleSelection(ctx *MsgContext, cmdArgs *CmdArgs) bool {
	if cmdArgs == nil || len(cmdArgs.Args) != 1 || len(cmdArgs.Kwargs) != 0 {
		return false
	}
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil || ctx.IsPrivate {
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
