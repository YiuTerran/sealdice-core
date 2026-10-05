//nolint:testpackage // These tests exercise private bridge policy and native solver behavior.
package dice

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sealdice-core/dice/service"
	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

func TestOnebotBridgePolicyMatchesSharedFixture(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	data, err := os.ReadFile("testdata/onebot-bridge-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Allowed []string `json:"allowed"`
		Denied  []string `json:"denied"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	check := func(command string, want bool) {
		t.Helper()
		msg := newGroupMsg("QQ-Group:88990001", "QQ:88990002", command)
		ctx := &MsgContext{Dice: d, EndPoint: ep, Session: d.ImSession, MessageType: "group"}
		if strings.HasPrefix(command, ".ban query QQ:") {
			target := strings.TrimPrefix(command, ".ban query ")
			d.Config.BanList.Map.Store(target, &BanListInfoItem{ID: target, Rank: BanRankNormal})
		}
		group := SetBotOnAtGroup(ctx, msg.GroupID)
		if strings.HasPrefix(command, ".dnd") || strings.HasPrefix(command, ".ss") || strings.HasPrefix(command, ".buff") ||
			strings.HasPrefix(command, ".ds") || strings.HasPrefix(command, ".死亡豁免") || strings.HasPrefix(command, ".init") {
			group.System = "dnd5e"
		} else {
			group.System = "coc7"
		}
		ctx.Group, ctx.Player = GetPlayerInfoBySender(ctx, msg)
		args := (&CmdArgs{}).commandParseNew(ctx, msg, true)
		got := false
		if args != nil {
			got = onebotBridgeInputAllowed(command) && onebotBridgeCommandAllowed(ctx, strings.ToLower(args.Command), args)
			if got && strings.EqualFold(args.Command, "set") {
				got = len(args.Args) == 1 && strings.EqualFold(args.Args[0], "info") || isOnebotBridgeRuleSelection(ctx, args)
			}
		}
		if got != want {
			t.Errorf("command %q parsed=%#v allowed=%v, want %v", command, args, got, want)
		}
	}
	for _, command := range fixture.Allowed {
		check(command, true)
	}
	for _, command := range fixture.Denied {
		check(command, false)
	}
	if onebotBridgeInputAllowed(strings.Repeat("x", 4001)) {
		t.Fatal("accepted a command longer than 4000 characters")
	}
	if !onebotBridgeInputAllowed(strings.Repeat("界", 4000)) {
		t.Fatal("rejected a command at the 4000-character limit")
	}
}

func TestOnebotBridgeSetCommandExecuteNewLifecycle(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	if err := d.DBOperator.GetDataDB(constant.WRITE).AutoMigrate(&model.GroupPlayerInfoBase{}); err != nil {
		t.Fatal(err)
	}
	em := &onebotBridgeCaptureEmitter{completeCh: make(chan onebotBridgeCompleteParams, 8)}
	pa := &PlatformAdapterOnebot{EndPoint: ep, LLMBridgeEnabled: true, logger: d.Logger}
	ep.Adapter = pa
	ep.Session = d.ImSession

	testCases := []struct {
		name       string
		audience   string
		command    string
		wantStatus string
		wantOutput bool
	}{
		{name: "group info", audience: "group", command: ".set info", wantStatus: "ok", wantOutput: true},
		{name: "private info", audience: "private", command: ".set info", wantStatus: "ok", wantOutput: true},
		{name: "group clear denied", audience: "group", command: ".set clr", wantStatus: "failed"},
		{name: "group numeric setting denied", audience: "group", command: ".set 123", wantStatus: "failed"},
		{name: "private rule selection denied", audience: "private", command: ".set coc", wantStatus: "failed"},
	}
	for i, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			const userID int64 = 22101
			const groupID int64 = 22102
			sourceID := int64(601 + i)
			_, tracker := newOnebotBridgeTestTracker(t, pa, em, testCase.audience, sourceID, userID, groupID, "set-info-"+strconvFormatInt(sourceID))
			var msg *Message
			if testCase.audience == "group" {
				msg = newGroupMsg("QQ-Group:"+strconvFormatInt(groupID), "QQ:"+strconvFormatInt(userID), testCase.command)
			} else {
				msg = newPrivateMsg("QQ:"+strconvFormatInt(userID), testCase.command)
			}
			msg.RawID = sourceID
			msg.LLMBridgeRequest = tracker

			em.mu.Lock()
			sendsBefore := len(em.sends)
			em.mu.Unlock()
			d.ImSession.ExecuteNew(ep, msg)
			// ExecuteNew's bridge branch is normally enclosed by the incoming
			// OneBot event's defer; mirror it here so rejected commands complete too.
			tracker.finishTask()

			var completion onebotBridgeCompleteParams
			select {
			case completion = <-em.completeCh:
			case <-time.After(2 * time.Second):
				t.Fatal("bridge command did not complete")
			}
			if completion.Status != testCase.wantStatus {
				t.Fatalf("completion status = %q, want %q: %#v", completion.Status, testCase.wantStatus, completion)
			}
			if testCase.wantOutput && completion.OutputCount == 0 {
				t.Fatalf("approved query completed without a native reply: %#v", completion)
			}
			if !testCase.wantOutput && completion.OutputCount != 0 {
				t.Fatalf("rejected state-changing command emitted output: %#v", completion)
			}
			em.mu.Lock()
			sendCount := len(em.sends) - sendsBefore
			em.mu.Unlock()
			if testCase.wantOutput && sendCount == 0 {
				t.Fatal("approved query was not delivered through the bridge emitter")
			}
			if !testCase.wantOutput && sendCount != 0 {
				t.Fatalf("rejected command sent %d replies", sendCount)
			}
		})
	}
}

func TestOnebotBridgeMasterACLIsBoundedToConnection(t *testing.T) {
	const connectionID = "master-acl-connection"
	userID := onebotBridgeMasterIDMin + 1
	conn := &onebotBridgeConnection{registerDone: make(chan struct{})}
	conn.setMasterAuthorization(connectionID, &onebotBridgeAuthorization{Version: 1, MasterUserIDs: []int64{userID}})
	conn.setRegistration(connectionID, true)
	if !conn.hasMasterUser(connectionID, userID) {
		t.Fatal("authorized master was not recognized")
	}
	if conn.hasMasterUser("old-connection", userID) {
		t.Fatal("authorization leaked to another connection")
	}
	conn.setRegistration("replacement-connection", true)
	if conn.hasMasterUser(connectionID, userID) || conn.hasMasterUser("replacement-connection", userID) {
		t.Fatal("re-registration retained an old connection ACL")
	}
	conn.setMasterAuthorization("replacement-connection", &onebotBridgeAuthorization{Version: 1})
	if conn.hasMasterUser("replacement-connection", userID) {
		t.Fatal("empty negotiated ACL authorized a user")
	}
	conn.setMasterAuthorization("replacement-connection", nil)
	if conn.hasMasterUser("replacement-connection", userID) {
		t.Fatal("missing ACL capability authorized a user")
	}
	for _, invalid := range [][]int64{{onebotBridgeMasterIDMin - 1}, {onebotBridgeMasterIDMax}, make([]int64, onebotBridgeMasterIDLimit+1)} {
		for i := range invalid {
			if len(invalid) > 1 {
				invalid[i] = userID
			}
		}
		conn.setMasterAuthorization("replacement-connection", &onebotBridgeAuthorization{Version: 1, MasterUserIDs: invalid})
		if conn.hasMasterUser("replacement-connection", userID) {
			t.Fatalf("invalid ACL %v authorized a user", invalid)
		}
	}
	conn.close()
	if conn.hasMasterUser("replacement-connection", userID) {
		t.Fatal("closed connection retained a master ACL")
	}
}

func TestOnebotBridgeMasterCommandRequiresPrivateOriginalAuthorizedSender(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	const userID = onebotBridgeMasterIDMin + 7
	conn := &onebotBridgeConnection{registerDone: make(chan struct{})}
	conn.setMasterAuthorization("current", &onebotBridgeAuthorization{Version: 1, MasterUserIDs: []int64{userID}})
	conn.setRegistration("current", true)
	adapter := &PlatformAdapterOnebot{LLMBridgeEnabled: true}
	msg := newPrivateMsg("QQ:"+strconvFormatInt(userID), ".master list")
	msg.RawID = 1
	tracker, err := newOnebotBridgeRequestTracker(adapter, conn, msg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &MsgContext{Dice: d, EndPoint: ep, Session: d.ImSession, MessageType: "private", IsPrivate: true, LLMBridgeRequest: tracker}
	ctx.Group, ctx.Player = onebotBridgePrivateQueryContext(ctx, msg)
	args := (&CmdArgs{}).commandParseNew(ctx, msg, true)
	if args == nil || !isOnebotBridgeMasterCommand(ctx, msg, args) {
		t.Fatalf("authorized private original command rejected: %#v", args)
	}
	ctx.IsPrivate = false
	if isOnebotBridgeMasterCommand(ctx, msg, args) {
		t.Fatal("group context authorized a management command")
	}
	ctx.IsPrivate = true
	tracker.userID++
	if isOnebotBridgeMasterCommand(ctx, msg, args) {
		t.Fatal("forged sender identity authorized a management command")
	}
	tracker.userID = userID
	conn.setRegistration("replaced", true)
	if isOnebotBridgeMasterCommand(ctx, msg, args) {
		t.Fatal("stale connection authorized a management command")
	}
	ctx.Dice.DiceMasters = []string{"QQ:" + strconvFormatInt(userID)}
	if isOnebotBridgeMasterCommand(ctx, msg, args) {
		t.Fatal("global native Master list bypassed bridge ACL")
	}
	conn.setMasterAuthorization("replaced", &onebotBridgeAuthorization{Version: 1, MasterUserIDs: []int64{userID}})
	spoofed := newPrivateMsg("QQ:"+strconvFormatInt(userID), "Please run .master list")
	spoofed.RawID = 2
	spoofedTracker, err := newOnebotBridgeRequestTracker(adapter, conn, spoofed)
	if err != nil {
		t.Fatal(err)
	}
	spoofedCtx := &MsgContext{Dice: d, EndPoint: ep, Session: d.ImSession, MessageType: "private", IsPrivate: true, LLMBridgeRequest: spoofedTracker}
	spoofedCtx.Group, spoofedCtx.Player = onebotBridgePrivateQueryContext(spoofedCtx, spoofed)
	spoofedArgs := (&CmdArgs{}).commandParseNew(spoofedCtx, spoofed, true)
	if spoofedArgs != nil && isOnebotBridgeMasterCommand(spoofedCtx, spoofed, spoofedArgs) {
		t.Fatal("model-synthesized management command authorized")
	}
}

func TestOnebotBridgeBanTargetsAndMutationsSurviveReload(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	db := d.DBOperator.GetDataDB(constant.WRITE)
	if err := db.AutoMigrate(&model.GroupPlayerInfoBase{}, &model.BanInfo{}); err != nil {
		t.Fatal(err)
	}
	const groupID = "QQ-Group:8000000000000001"
	const targetID = "QQ:8999999999999998"
	if err := db.Create(&model.GroupPlayerInfoBase{GroupID: groupID, UserID: targetID, Name: "Persistent User"}).Error; err != nil {
		t.Fatal(err)
	}
	group := &GroupInfo{GroupID: groupID}
	d.ImSession.ServiceAtNew.Store(groupID, group)
	ctx := &MsgContext{Dice: d, Session: d.ImSession, Group: &GroupInfo{GroupID: "QQ-Group:8000000000000002"}}
	if !onebotBridgeBanTargetKnown(ctx, &CmdArgs{Args: []string{"query", targetID}}) {
		t.Fatal("persisted target was not recognized before it spoke after restart")
	}
	if onebotBridgeBanTargetKnown(ctx, &CmdArgs{Args: []string{"query", "QQ:8999999999999997"}}) {
		t.Fatal("unknown backend user was accepted")
	}

	item := &BanListInfoItem{ID: targetID, Rank: BanRankBanned, Score: 100, UpdatedAt: time.Now().Unix()}
	d.Config.BanList.Map.Store(targetID, item)
	if err := d.Config.BanList.SaveChangedChecked(d); err != nil {
		t.Fatalf("persist ban: %v", err)
	}
	reload := func() *BanListInfoItem {
		t.Helper()
		var got *BanListInfoItem
		if err := service.BanItemList(d.DBOperator, func(id string, banUpdatedAt int64, data []byte) {
			if id != targetID {
				return
			}
			var decoded BanListInfoItem
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Errorf("decode persisted ban: %v", err)
				return
			}
			decoded.BanUpdatedAt = banUpdatedAt
			got = &decoded
		}); err != nil {
			t.Fatalf("reload ban table: %v", err)
		}
		return got
	}
	if got := reload(); got == nil || got.Rank != BanRankBanned {
		t.Fatalf("persisted ban = %#v, want banned after restart", got)
	}
	item.Rank = BanRankNormal
	item.Score = 0
	item.UpdatedAt = time.Now().Unix()
	if err := d.Config.BanList.SaveChangedChecked(d); err != nil {
		t.Fatalf("persist ban removal: %v", err)
	}
	if got := reload(); got == nil || got.Rank != BanRankNormal {
		t.Fatalf("persisted removal = %#v, want normal after restart", got)
	}
}

func TestOnebotBridgeReadOnlyQueriesDoNotPersistPrivateGroupOrSwitchRules(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	if err := d.DBOperator.GetDataDB(constant.WRITE).AutoMigrate(&model.GroupPlayerInfoBase{}); err != nil {
		t.Fatal(err)
	}
	msg := newPrivateMsg("QQ:8999999999999997", ".userid")
	ctx := &MsgContext{Dice: d, EndPoint: ep, Session: d.ImSession, MessageType: "private", IsPrivate: true, LLMBridgeRequest: &onebotBridgeRequestTracker{}}
	ctx.Group, ctx.Player = onebotBridgePrivateQueryContext(ctx, msg)
	ctx.LLMBridgeReadOnly = true
	if _, exists := d.ImSession.ServiceAtNew.Load("PG-" + msg.Sender.UserID); exists {
		t.Fatal("private query created a persisted PG group")
	}
	SetTempVars(ctx, msg.Sender.Nickname)
	if ctx.Group.System != "" {
		t.Fatalf("private query assigned default system %q", ctx.Group.System)
	}
	if _, exists := d.DirtyGroups.Load(ctx.Group.GroupID); exists {
		t.Fatal("private query marked a group dirty")
	}
	if !onebotBridgeBanTargetKnown(ctx, &CmdArgs{Args: []string{"query", msg.Sender.UserID}}) {
		t.Fatal("a sender previously observed by private .userid was not recognized as a known backend user")
	}
	if service.GroupPlayerInfoGet(d.DBOperator, "PG-"+msg.Sender.UserID, msg.Sender.UserID) == nil {
		t.Fatal("private identity metadata was not persisted")
	}

	group := &GroupInfo{GroupID: "QQ-Group:8800000000000001", System: "dnd5e", CocRuleIndex: 3}
	queryCtx := &MsgContext{Dice: d, EndPoint: ep, Session: d.ImSession, MessageType: "group", Group: group, LLMBridgeRequest: &onebotBridgeRequestTracker{}}
	query := newGroupMsg(group.GroupID, "QQ:8999999999999997", ".setcoc details")
	setcoc := d.ExtFind("coc7", false).GetCmdMap()["setcoc"]
	if setcoc == nil {
		t.Fatal("COC setcoc command is unavailable")
	}
	result := setcoc.Solve(queryCtx, query, &CmdArgs{Command: "setcoc", Args: []string{"details"}})
	if !result.Solved || group.System != "dnd5e" || group.CocRuleIndex != 3 {
		t.Fatalf(".setcoc details mutated group state: result=%#v group=%#v", result, group)
	}
}

func TestOnebotBridgePrivateQueryContextCopiesExistingPrivateGroup(t *testing.T) {
	d, ep, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	if err := d.DBOperator.GetDataDB(constant.WRITE).AutoMigrate(&model.GroupPlayerInfoBase{}); err != nil {
		t.Fatal(err)
	}

	msg := newPrivateMsg("QQ:8999999999999997", ".userid")
	groupID := "PG-" + msg.Sender.UserID
	source := &GroupInfo{
		GroupID:           groupID,
		System:            "dnd5e",
		InactivatedExtSet: StringSet{"disabled-extension": {}},
		ExtAppliedTime:    1,
	}
	source.activatedExtList = []*ExtInfo{{Name: "unavailable-extension"}}
	// Simulate a PG group created by an earlier private command such as .r or
	// .master backup; subsequent queries copy its extension state.
	d.ImSession.ServiceAtNew.Store(groupID, source)

	ctx := &MsgContext{
		Dice:        d,
		EndPoint:    ep,
		Session:     d.ImSession,
		MessageType: "private",
		IsPrivate:   true,
	}
	group, player := onebotBridgePrivateQueryContext(ctx, msg)
	if group == nil || player == nil {
		t.Fatalf("private query context = group %#v, player %#v", group, player)
	}
	if group.System != source.System || !group.IsExtInactivated("disabled-extension") {
		t.Fatalf("private query did not copy existing group settings: %#v", group)
	}
	if _, dirty := d.DirtyGroups.Load(groupID); dirty {
		t.Fatal("private query marked the existing PG group dirty")
	}
	foundExistingExtension := false
	for _, extension := range group.activatedExtList {
		if extension == source.activatedExtList[0] {
			foundExistingExtension = true
			break
		}
	}
	if !foundExistingExtension {
		t.Fatalf("private query did not copy existing extension state: %#v", group.activatedExtList)
	}
	if len(source.activatedExtList) != 1 || source.activatedExtList[0].Name != "unavailable-extension" {
		t.Fatalf("private query mutated source extension state: %#v", source.activatedExtList)
	}
}

func TestOnebotBridgeBanMutationPersistsOnlyTargetAndRollsBackOnFailure(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	db := d.DBOperator.GetDataDB(constant.WRITE)
	if err := db.AutoMigrate(&model.BanInfo{}); err != nil {
		t.Fatal(err)
	}
	const targetID = "QQ:8999999999999998"
	const unrelatedID = "QQ:8999999999999997"
	target := &BanListInfoItem{ID: targetID, Name: "target", Rank: BanRankNormal, UpdatedAt: time.Now().Unix()}
	d.Config.BanList.Map.Store(targetID, target)
	if err := d.Config.BanList.SaveItemChecked(d, targetID); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	d.Config.BanList.Map.Store(unrelatedID, &BanListInfoItem{ID: unrelatedID, Rank: BanRankNormal, UpdatedAt: time.Now().Unix()})
	if err := db.Exec("CREATE TRIGGER reject_unrelated_ban BEFORE INSERT ON ban_info WHEN NEW.id = 'QQ:8999999999999997' BEGIN SELECT RAISE(ABORT, 'unrelated write rejected'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := persistOnebotBridgeBanMutation(d, targetID, func() {
		item, _ := d.Config.BanList.Map.Load(targetID)
		item.Rank = BanRankBanned
		item.Score = 200
		item.UpdatedAt = time.Now().Unix()
	}); err != nil {
		t.Fatalf("target mutation failed because of unrelated dirty entry: %v", err)
	}
	if item, _ := d.Config.BanList.Map.Load(unrelatedID); item == nil || item.UpdatedAt == 0 {
		t.Fatal("target-scoped mutation unexpectedly flushed the unrelated dirty entry")
	}

	if err := db.Exec("CREATE TRIGGER reject_target_update BEFORE UPDATE ON ban_info WHEN OLD.id = 'QQ:8999999999999998' BEGIN SELECT RAISE(ABORT, 'target write rejected'); END").Error; err != nil {
		t.Fatal(err)
	}
	before, _ := d.Config.BanList.Map.Load(targetID)
	before = cloneBanListInfoItem(before)
	err := persistOnebotBridgeBanMutation(d, targetID, func() {
		item, _ := d.Config.BanList.Map.Load(targetID)
		item.Rank = BanRankTrusted
		item.Score = 0
		item.Reasons = append(item.Reasons, "must roll back")
		item.UpdatedAt = time.Now().Unix()
	})
	if err == nil {
		t.Fatal("triggered target persistence failure was reported as success")
	}
	after, _ := d.Config.BanList.Map.Load(targetID)
	if after.Rank != before.Rank || after.Score != before.Score || strings.Join(after.Reasons, ",") != strings.Join(before.Reasons, ",") {
		t.Fatalf("failed mutation was not rolled back: before=%#v after=%#v", before, after)
	}
	var persisted model.BanInfo
	if err := db.Where("id = ?", targetID).First(&persisted).Error; err != nil {
		t.Fatalf("load target after failed mutation: %v", err)
	}
	var decoded BanListInfoItem
	if err := json.Unmarshal(persisted.Data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Rank != before.Rank || decoded.Score != before.Score {
		t.Fatalf("failed mutation changed persisted target: %#v", decoded)
	}
}

func TestOnebotBridgeBackupVerifierChecksNativeArchive(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	if err := os.WriteFile(filepath.Join(d.BaseConfig.DataDir, "serve.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(d.BaseConfig.DataDir, "data.db")
	dataDB, openErr := openTestGormDB(dbPath)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if sqlDB, dbErr := dataDB.DB(); dbErr == nil {
		_ = sqlDB.Close()
	}
	backupRoot := t.TempDir()
	t.Chdir(backupRoot)
	if err := os.MkdirAll(filepath.Join(backupRoot, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupRoot, "data", "dice.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.BaseConfig.DataDir, "data-logs.db"), []byte("log db"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupPath, backupErr := d.Parent.Backup(BackupSelectionBasic, false)
	if backupErr != nil {
		t.Fatalf("create native backup: %v", backupErr)
	}
	if verifyErr := verifyOnebotBridgeBackup(backupPath, d.BaseConfig.DataDir); verifyErr != nil {
		t.Fatalf("native backup did not pass verification: %v", verifyErr)
	}
	if err := os.WriteFile(filepath.Join(backupRoot, backupPath), []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if verifyErr := verifyOnebotBridgeBackup(backupPath, d.BaseConfig.DataDir); verifyErr == nil {
		t.Fatal("corrupted native backup passed verification")
	}
}

func TestOnebotBridgeMasterBackupSolverWithHeadlessAllSelection(t *testing.T) {
	d, ep, adapter, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	d.Parent.AutoBackupSelection = BackupSelectionAll

	root := t.TempDir()
	t.Chdir(root)
	globalDataDir := filepath.Join(root, "data")
	diceDataDir := filepath.Join(globalDataDir, "default")
	if err := os.MkdirAll(diceDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(globalDataDir, "dice.yaml"):     "{}\n",
		filepath.Join(diceDataDir, "serve.yaml"):      "{}\n",
		filepath.Join(diceDataDir, "data.db"):         "test data db",
		filepath.Join(diceDataDir, "data-logs.db"):    "test log db",
		filepath.Join(diceDataDir, "configs", "seed"): "optional config directory",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d.BaseConfig.DataDir = diceDataDir

	msg := newPrivateMsg("QQ:8999999999999998", ".master backup")
	ctx := &MsgContext{
		Dice:             d,
		MessageType:      "private",
		IsPrivate:        true,
		EndPoint:         ep,
		Session:          d.ImSession,
		PrivilegeLevel:   100,
		LLMBridgeRequest: &onebotBridgeRequestTracker{},
		Group:            &GroupInfo{GroupID: "PG-" + msg.Sender.UserID},
		Player:           &GroupPlayerInfo{UserID: msg.Sender.UserID},
	}
	args := (&CmdArgs{}).commandParseNew(ctx, msg, true)
	if args == nil || args.Command != "master" {
		t.Fatalf("native master command parser returned %#v", args)
	}
	command := d.CmdMap["master"]
	if command == nil {
		t.Fatal("native master solver is unavailable")
	}
	result := command.Solve(ctx, msg, args)
	if !result.Matched || !result.Solved {
		t.Fatalf("native master solver result = %#v", result)
	}

	adapter.mu.Lock()
	var backupSucceeded bool
	for _, text := range adapter.personMsgs {
		if strings.Contains(text, "本地备份成功") {
			backupSucceeded = true
		}
	}
	adapter.mu.Unlock()
	if !backupSucceeded {
		t.Fatalf("headless .master backup did not report verified success; replies=%q", adapter.personMsgs)
	}

	for path, content := range map[string]string{
		filepath.Join(root, "data", "decks", "test.deck"): "deck payload",
		filepath.Join(root, "data", "images", "test.png"): "image payload",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resourceBackup, backupErr := d.Parent.Backup(BackupSelectionAll, true)
	if backupErr != nil {
		t.Fatalf("backup with existing resources failed: %v", backupErr)
	}
	archiveFile, openErr := os.Open(filepath.Join(root, resourceBackup))
	if openErr != nil {
		t.Fatal(openErr)
	}
	archiveInfo, statErr := archiveFile.Stat()
	if statErr != nil {
		_ = archiveFile.Close()
		t.Fatal(statErr)
	}
	resourceArchive, zipErr := zip.NewReader(archiveFile, archiveInfo.Size())
	_ = archiveFile.Close()
	if zipErr != nil {
		t.Fatal(zipErr)
	}
	entries := make(map[string]struct{}, len(resourceArchive.File))
	for _, entry := range resourceArchive.File {
		entries[entry.Name] = struct{}{}
	}
	for _, wanted := range []string{"data/decks/test.deck", "data/images/test.png"} {
		if _, exists := entries[wanted]; !exists {
			t.Errorf("backup archive omitted existing resource %q", wanted)
		}
	}
}

func TestDiceManagerBackupReturnsOptionalResourceStatErrors(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()
	root := t.TempDir()
	t.Chdir(root)
	globalDataDir := filepath.Join(root, "data")
	diceDataDir := filepath.Join(globalDataDir, "default")
	if err := os.MkdirAll(diceDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(globalDataDir, "dice.yaml"):  "{}\n",
		filepath.Join(diceDataDir, "serve.yaml"):   "{}\n",
		filepath.Join(diceDataDir, "data.db"):      "test data db",
		filepath.Join(diceDataDir, "data-logs.db"): "test log db",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d.BaseConfig.DataDir = diceDataDir
	for _, link := range []string{
		filepath.Join(globalDataDir, "helpdoc"),
		filepath.Join(diceDataDir, "advanced.yaml"),
	} {
		if err := os.Symlink(filepath.Base(link), link); err != nil {
			t.Skipf("symlinks unavailable for stat error regression: %v", err)
		}
	}

	_, backupErr := d.Parent.Backup(BackupSelectionHelpDoc, false)
	if backupErr == nil || !strings.Contains(backupErr.Error(), "helpdoc") || !strings.Contains(backupErr.Error(), "advanced.yaml") {
		t.Fatalf("non-ENOENT resource stat errors were not reported: %v", backupErr)
	}
}
