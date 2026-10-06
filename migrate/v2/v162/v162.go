package v162

import (
	"fmt"

	"sealdice-core/model"
	"sealdice-core/utils/constant"
	operator "sealdice-core/utils/dboperator/engine"
	upgrade "sealdice-core/utils/upgrader"
)

// V162OnebotBridgeLogEventsMigration creates the durable idempotency ledger
// used by the optional OneBot log capture capability.
var V162OnebotBridgeLogEventsMigration = upgrade.Upgrade{
	ID: "013_V162OnebotBridgeLogEventsMigration",
	Description: `
# Upgrade note
Create the durable OneBot bridge log-capture event ledger.
`,
	Apply: func(logf func(string), dbOperator operator.DatabaseOperator) error {
		logf("[INFO] V162 OneBot bridge log-capture ledger migration started")
		if err := dbOperator.GetLogDB(constant.WRITE).AutoMigrate(&model.OnebotBridgeLogEvent{}, &model.OnebotBridgeLogState{}); err != nil {
			return fmt.Errorf("create OneBot bridge log-capture ledger: %w", err)
		}
		logf("[INFO] V162 OneBot bridge log-capture ledger migration completed")
		return nil
	},
}
