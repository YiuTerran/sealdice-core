package v162_test

import (
	"testing"

	v162 "sealdice-core/migrate/v2/v162"
	"sealdice-core/migrate/v2/v2test"
	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

func TestV162AddsBridgeTablesWithoutChangingExistingUserData(t *testing.T) {
	op, _ := v2test.NewTestSQLiteEngine(t)
	logDB := op.GetLogDB(constant.WRITE)
	dataDB := op.GetDataDB(constant.WRITE)
	v2test.MustExec(t, logDB, `CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT, group_id TEXT, created_at INTEGER, updated_at INTEGER, size INTEGER,
		extra TEXT, upload_url TEXT, upload_time INTEGER
	)`)
	v2test.MustExec(t, logDB, `CREATE TABLE log_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		log_id INTEGER, group_id TEXT, nickname TEXT, im_userid TEXT, time INTEGER,
		message TEXT, is_dice BOOLEAN, command_id INTEGER, command_info TEXT,
		raw_msg_id TEXT, user_uniform_id TEXT, removed INTEGER, parent_id INTEGER
	)`)
	v2test.MustExec(t, logDB, `INSERT INTO logs (id, name, group_id, created_at, updated_at, size, upload_url)
		VALUES (42, 'existing-log', 'QQ-Group:8000000000000001', 10, 20, 1, 'legacy')`)
	v2test.MustExec(t, logDB, `INSERT INTO log_items (id, log_id, group_id, nickname, im_userid, time, message, is_dice, raw_msg_id, user_uniform_id)
		VALUES (73, 42, 'QQ-Group:8000000000000001', 'Member', '8000000000000002', 15, 'old card/log body', 0, 'legacy-event', 'OneBot:8000000000000002')`)
	if err := dataDB.AutoMigrate(&model.GroupInfo{}); err != nil {
		t.Fatalf("create existing group data table: %v", err)
	}
	legacyData := []byte(`{"cards":{"active":"existing-card"},"settings":{"keep":true}}`)
	if err := dataDB.Exec("INSERT INTO group_info (id, created_at, data) VALUES (?, ?, ?)", "QQ-Group:8000000000000001", 1, legacyData).Error; err != nil {
		t.Fatalf("seed existing group data: %v", err)
	}

	if err := v162.V162OnebotBridgeLogEventsMigration.Apply(v2test.SilentLogf, op); err != nil {
		t.Fatalf("apply V162 migration: %v", err)
	}
	var logCount, itemCount int64
	if err := logDB.Model(&model.LogInfo{}).Where("id = ? AND name = ? AND group_id = ? AND upload_url = ?", 42, "existing-log", "QQ-Group:8000000000000001", "legacy").Count(&logCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := logDB.Model(&model.LogOneItem{}).Where("id = ? AND log_id = ? AND message = ? AND raw_msg_id = ?", 73, 42, "old card/log body", "legacy-event").Count(&itemCount).Error; err != nil {
		t.Fatal(err)
	}
	if logCount != 1 || itemCount != 1 {
		t.Fatalf("existing logs/items changed: logs=%d items=%d", logCount, itemCount)
	}
	var gotGroup model.GroupInfo
	if err := dataDB.Take(&gotGroup, "id = ?", "QQ-Group:8000000000000001").Error; err != nil {
		t.Fatal(err)
	}
	if string(gotGroup.Data) != string(legacyData) {
		t.Fatalf("existing card/group data changed: %s", gotGroup.Data)
	}
	var events, states int64
	if err := logDB.Model(&model.OnebotBridgeLogEvent{}).Count(&events).Error; err != nil {
		t.Fatal(err)
	}
	if err := logDB.Model(&model.OnebotBridgeLogState{}).Where("is_on = ?", true).Count(&states).Error; err != nil {
		t.Fatal(err)
	}
	if events != 0 || states != 0 {
		t.Fatalf("bridge migration must not backfill or enable capture: events=%d active-states=%d", events, states)
	}
}
