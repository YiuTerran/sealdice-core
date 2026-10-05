package dice

import (
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
	dataDB, err := openTestGormDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := dataDB.DB(); err == nil {
		_ = sqlDB.Close()
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	backupRoot := t.TempDir()
	if err := os.Chdir(backupRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	backupPath, err := d.Parent.Backup(BackupSelectionBasic, false)
	if err != nil {
		t.Fatalf("create native backup: %v", err)
	}
	if err := verifyOnebotBridgeBackup(backupPath, d.BaseConfig.DataDir); err != nil {
		t.Fatalf("native backup did not pass verification: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupRoot, backupPath), []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyOnebotBridgeBackup(backupPath, d.BaseConfig.DataDir); err == nil {
		t.Fatal("corrupted native backup passed verification")
	}
}
