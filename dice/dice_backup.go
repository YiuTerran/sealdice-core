package dice

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexmullins/zip"

	"sealdice-core/dice/service"
	"sealdice-core/logger"
	"sealdice-core/utils"
	"sealdice-core/utils/constant"
	"sealdice-core/utils/crypto"
)

const BackupDir = "./backups"

type BackupCleanStrategy int

const (
	BackupCleanStrategyDisabled BackupCleanStrategy = iota
	BackupCleanStrategyByCount
	BackupCleanStrategyByTime
)

type BackupCleanTrigger int

const (
	// BackupCleanTriggerCron 通过独立定时任务触发
	BackupCleanTriggerCron BackupCleanTrigger = 1 << iota
	// BackupCleanTriggerRotate 通过自动备份触发
	BackupCleanTriggerRotate
)

type backupConfigGlobal struct {
	Global  bool                         `json:"global"`
	Decks   bool                         `json:"decks"`
	HelpDoc bool                         `json:"helpDoc"`
	Censor  bool                         `json:"censor"`
	Names   bool                         `json:"names"`
	Images  bool                         `json:"images"`
	Dices   map[string]*backupConfigDice `json:"dices"`
}

type backupConfigDice struct {
	Accounts    bool `json:"accounts"`    // 帐号
	MiscConfig  bool `json:"miscConfig"`  // 综合设置
	PlayerData  bool `json:"playerData"`  // 用户数据
	CustomReply bool `json:"customReply"` // 文案模板
	CustomText  bool `json:"customText"`  // 自定义回复
	JSScripts   bool `json:"jsScripts"`   // JS脚本
}

type BackupSelection uint64

const (
	BackupSelectionJS BackupSelection = 1 << iota
	BackupSelectionDecks
	BackupSelectionHelpDoc
	BackupSelectionCensor
	BackupSelectionNames
	BackupSelectionImages

	BackupSelectionBasic     BackupSelection = 0
	BackupSelectionResources BackupSelection = BackupSelectionImages
	BackupSelectionAll       BackupSelection = BackupSelectionBasic |
		BackupSelectionJS |
		BackupSelectionDecks |
		BackupSelectionHelpDoc |
		BackupSelectionCensor |
		BackupSelectionNames |
		BackupSelectionResources
)

func (dm *DiceManager) Backup(sel BackupSelection, fromAuto bool) (string, error) {
	if dm == nil || len(dm.Dice) == 0 || dm.Dice[0] == nil {
		return "", errors.New("no Dice instance is available for backup")
	}
	if err := os.MkdirAll(BackupDir, 0o755); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	backupLogger := dm.Dice[0].Logger
	if backupLogger == nil {
		backupLogger = logger.M()
	}

	cfgGlb := backupConfigGlobal{
		Global: true,
		Dices:  map[string]*backupConfigDice{},
	}
	cfgDice := backupConfigDice{
		Accounts:    true,
		MiscConfig:  true,
		PlayerData:  true,
		CustomReply: true,
		CustomText:  true,
	}

	bakFn := "bak_" + time.Now().Format("060102_150405")
	if fromAuto {
		bakFn += "_auto"
	}
	bakFn += "_r" + strconv.FormatUint(uint64(sel), 16)
	fnHashed := crypto.CalculateSHA512Str([]byte(bakFn))[:8]
	bakFn += "_" + fnHashed + ".zip"

	fzip, err := os.OpenFile(filepath.Join(BackupDir, bakFn),
		os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}

	writer := zip.NewWriter(fzip)
	archiveClosed := false
	defer func() {
		if !archiveClosed {
			_ = writer.Close()
			_ = fzip.Close()
		}
	}()
	var backupErrors []error
	recordBackupError := func(d *Dice, filename string, err error) {
		if err == nil {
			return
		}
		backupErrors = append(backupErrors, fmt.Errorf("backup %s: %w", filename, err))
		if d != nil && d.Logger != nil {
			d.Logger.Errorf("备份文件失败: %s, 原因: %s", filename, err.Error())
		} else {
			backupLogger.Errorf("备份文件失败: %s, 原因: %s", filename, err.Error())
		}
	}
	optionalDirectory := func(root string) (bool, error) {
		info, err := os.Stat(root)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, fmt.Errorf("%s is not a directory", root)
		}
		return true, nil
	}
	optionalFile := func(root string) (bool, error) {
		info, err := os.Stat(root)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.IsDir() {
			return false, fmt.Errorf("%s is a directory", root)
		}
		return true, nil
	}
	walkFilesIfPresent := func(root string, walkFn filepath.WalkFunc) error {
		exists, err := optionalDirectory(root)
		if err != nil || !exists {
			return err
		}
		return filepath.Walk(root, walkFn)
	}
	walkDirsIfPresent := func(root string, walkFn fs.WalkDirFunc) error {
		exists, err := optionalDirectory(root)
		if err != nil || !exists {
			return err
		}
		return filepath.WalkDir(root, walkFn)
	}

	backup := func(d *Dice, fn string) {
		file, err := os.Open(fn)
		if errors.Is(err, fs.ErrNotExist) && strings.HasSuffix(filepath.ToSlash(fn), "/session.token") {
			// In-pack client session tokens are optional and must never cause a
			// nil-file panic when backing up a headless/container instance.
			return
		}
		if err != nil {
			recordBackupError(d, fn, err)
			return
		}

		h := &zip.FileHeader{Name: fn, Method: zip.Deflate, Flags: 0x800}
		fileWriter, err := writer.CreateHeader(h)
		if err != nil {
			recordBackupError(d, fn, errors.Join(err, file.Close()))
			return
		}

		_, copyErr := io.Copy(fileWriter, file)
		closeErr := file.Close()
		recordBackupError(d, fn, errors.Join(copyErr, closeErr))
	}

	backupDir := func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info == nil {
			return fmt.Errorf("missing file information for %s", path)
		}
		if !info.IsDir() {
			backup(nil, path)
		}
		return nil
	}

	backup(nil, "data/dice.yaml")

	if sel&BackupSelectionDecks != 0 {
		cfgGlb.Decks = true
		walkErr := walkFilesIfPresent("data/decks", func(path string, info fs.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info == nil {
				return fmt.Errorf("missing file information for %s", path)
			}
			if !info.IsDir() {
				backup(nil, path)
				return nil
			}
			base := filepath.Base(path)
			// 跳过 deck 压缩包解压出的目录
			if strings.HasPrefix(base, "_") && strings.HasSuffix(base, ".deck") {
				deckArchive := filepath.Join(filepath.Dir(path), base[1:])
				if exists, statErr := optionalFile(deckArchive); statErr != nil {
					recordBackupError(nil, deckArchive, statErr)
				} else if exists {
					return filepath.SkipDir
				}
			}
			return nil
		})
		recordBackupError(nil, "data/decks", walkErr)
	}

	if sel&BackupSelectionHelpDoc != 0 {
		if exists, statErr := optionalDirectory("data/helpdoc"); statErr != nil {
			recordBackupError(nil, "data/helpdoc", statErr)
		} else if !exists {
			backupLogger.Warn("备份 helpdoc 失败: 不存在或不是目录")
		} else {
			cfgGlb.HelpDoc = true
			recordBackupError(nil, "data/helpdoc", walkFilesIfPresent("data/helpdoc", backupDir))
		}
	}

	if sel&BackupSelectionCensor != 0 {
		if exists, statErr := optionalDirectory("data/censor"); statErr != nil {
			recordBackupError(nil, "data/censor", statErr)
		} else if !exists {
			backupLogger.Warn("备份 censor 失败: 不存在或不是目录")
		} else {
			cfgGlb.Censor = true
			recordBackupError(nil, "data/censor", walkFilesIfPresent("data/censor", backupDir))
		}
	}

	if sel&BackupSelectionNames != 0 {
		if exists, statErr := optionalDirectory("data/names"); statErr != nil {
			recordBackupError(nil, "data/names", statErr)
		} else if !exists {
			backupLogger.Warn("备份 names 失败: 不存在或不是目录")
		} else {
			cfgGlb.Names = true
			recordBackupError(nil, "data/names", walkFilesIfPresent("data/names", backupDir))
		}
	}

	if sel&BackupSelectionImages != 0 {
		if exists, statErr := optionalDirectory("data/images"); statErr != nil {
			recordBackupError(nil, "data/images", statErr)
		} else if !exists {
			backupLogger.Warn("备份 images 失败: 不存在或不是目录")
		} else {
			cfgGlb.Images = true
			recordBackupError(nil, "data/images", walkFilesIfPresent("data/images", backupDir))
		}
	}

	withJS := sel&BackupSelectionJS != 0
	cfgDice.JSScripts = withJS

	for _, d := range dm.Dice {
		if d == nil {
			recordBackupError(nil, "Dice instance", errors.New("nil Dice instance"))
			continue
		}
		cfgGlb.Dices[d.BaseConfig.Name] = &cfgDice
		dataDir := d.BaseConfig.DataDir
		if d.DBOperator == nil {
			recordBackupError(d, dataDir, errors.New("database operator is unavailable"))
			continue
		}

		backup(d, filepath.Join(dataDir, "serve.yaml"))
		advancedConfig := filepath.Join(dataDir, "advanced.yaml")
		if exists, statErr := optionalFile(advancedConfig); statErr != nil {
			recordBackupError(d, advancedConfig, statErr)
		} else if exists {
			backup(d, advancedConfig)
		}
		pluginConfig := filepath.Join(dataDir, "configs", "plugin-configs.json")
		if exists, statErr := optionalFile(pluginConfig); statErr != nil {
			recordBackupError(d, pluginConfig, statErr)
		} else if exists {
			backup(d, pluginConfig)
		}

		err := service.FlushWAL(d.DBOperator.GetDataDB(constant.WRITE))
		if err != nil {
			recordBackupError(d, filepath.Join(dataDir, "data.db"), err)
		} else {
			backup(d, filepath.Join(dataDir, "data.db"))
		}
		err = service.FlushWAL(d.DBOperator.GetLogDB(constant.WRITE))
		if err != nil {
			recordBackupError(d, filepath.Join(dataDir, "data-logs.db"), err)
		} else {
			backup(d, filepath.Join(dataDir, "data-logs.db"))
		}
		if d.CensorManager != nil && d.CensorManager.DB != nil {
			err = service.FlushWAL(d.DBOperator.GetCensorDB(constant.WRITE))
			if err != nil {
				recordBackupError(d, filepath.Join(dataDir, "data-censor.db"), err)
			} else {
				backup(d, filepath.Join(dataDir, "data-censor.db"))
			}
		}

		textTemplate := filepath.Join(dataDir, "configs/text-template.yaml")
		if exists, statErr := optionalFile(textTemplate); statErr != nil {
			recordBackupError(d, textTemplate, statErr)
		} else if exists {
			backup(d, textTemplate)
		}

		replyWalkErr := walkDirsIfPresent(filepath.Join(dataDir, "extensions/reply"), func(path string, info fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info == nil {
				return fmt.Errorf("missing file information for %s", path)
			}
			// NOTE(Xiangze Li): copied from dice.ReplyReload. Should extract as function, but I'm lazy
			if info.IsDir() {
				if strings.EqualFold(info.Name(), "assets") || strings.EqualFold(info.Name(), "images") {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(info.Name(), ".reply") || info.Name() == "info.yaml" {
				return nil
			}

			ext := filepath.Ext(path)
			if ext == ".yaml" || ext == "" {
				backup(d, path)
			}
			return nil
		})
		recordBackupError(d, filepath.Join(dataDir, "extensions/reply"), replyWalkErr)

		if d.ImSession != nil {
			for _, i := range d.ImSession.EndPoints {
				if i.Platform == "QQ" {
					if pa, ok := i.Adapter.(*PlatformAdapterGocq); ok && pa.UseInPackClient {
						workDir := i.RelWorkDir
						if pa.BuiltinMode == "lagrange" {
							backup(d, filepath.Join(dataDir, workDir, "appsettings.json"))
							backup(d, filepath.Join(dataDir, workDir, "device.json"))
							backup(d, filepath.Join(dataDir, workDir, "keystore.json"))
						} else {
							backup(d, filepath.Join(dataDir, workDir, "config.yml"))
							backup(d, filepath.Join(dataDir, workDir, "device.json"))
							backup(d, filepath.Join(dataDir, workDir, "session.token"))
						}
					}
				}
			}
		}

		if withJS {
			scriptsWalkErr := walkDirsIfPresent(filepath.Join(dataDir, "scripts"), func(path string, info fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if info == nil {
					return fmt.Errorf("missing file information for %s", path)
				}
				if info.IsDir() {
					if info.Name() == "_builtin" {
						return filepath.SkipDir
					}
					return nil
				}
				if filepath.Ext(info.Name()) == ".js" {
					backup(d, path)
				}
				return nil
			})
			recordBackupError(d, filepath.Join(dataDir, "scripts"), scriptsWalkErr)
			extDataDir := filepath.Join(dataDir, "extensions")
			extWalkErr := walkDirsIfPresent(extDataDir, func(path string, info fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if info == nil {
					return fmt.Errorf("missing file information for %s", path)
				}
				if info.IsDir() {
					if filepath.Dir(path) == extDataDir {
						if ext := d.ExtFind(info.Name(), false); ext == nil || !ext.IsJsExt {
							return filepath.SkipDir
						}
					}
					return nil
				}
				backup(d, path)
				return nil
			})
			recordBackupError(d, extDataDir, extWalkErr)
		}
	}

	// 写入文件信息
	data, marshalErr := json.Marshal(map[string]interface{}{
		"config":      cfgGlb,
		"version":     VERSION.String(),
		"versionCode": VERSION_CODE,
	})
	if marshalErr != nil {
		recordBackupError(nil, "backup_info.json", marshalErr)
	} else {
		h := &zip.FileHeader{Name: "backup_info.json", Method: zip.Deflate, Flags: 0x800}
		fileWriter, headerErr := writer.CreateHeader(h)
		if headerErr != nil {
			recordBackupError(nil, "backup_info.json", headerErr)
		} else if n, writeErr := fileWriter.Write(data); writeErr != nil {
			recordBackupError(nil, "backup_info.json", writeErr)
		} else if n != len(data) {
			recordBackupError(nil, "backup_info.json", io.ErrShortWrite)
		}
	}
	writerErr := writer.Close()
	syncErr := fzip.Sync()
	fileCloseErr := fzip.Close()
	archiveClosed = true
	recordBackupError(nil, bakFn, errors.Join(writerErr, syncErr, fileCloseErr))
	if err := errors.Join(backupErrors...); err != nil {
		return "", err
	}

	return fzip.Name(), nil
}

func (dm *DiceManager) BackupAuto() error {
	_, err := dm.Backup(dm.AutoBackupSelection, true)
	return err
}

func (dm *DiceManager) BackupClean(fromAuto bool) (err error) {
	if dm.BackupCleanStrategy == BackupCleanStrategyDisabled {
		return nil
	}

	if fromAuto && (dm.BackupCleanTrigger&BackupCleanTriggerRotate == 0) {
		return nil
	}

	log := logger.M()
	log.Info("开始清理备份文件")

	backupDir, err := os.Open(BackupDir)
	if err != nil {
		return err
	}
	defer func() { _ = backupDir.Close() }()
	if i, _ := backupDir.Stat(); !i.IsDir() {
		return fmt.Errorf("backup directory %q is not a directory", BackupDir)
	}

	files, err := backupDir.ReadDir(-1)
	if err != nil {
		return err
	}

	fileInfos := make([]os.FileInfo, 0, len(files))
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if fi, err := f.Info(); err == nil {
			fileInfos = append(fileInfos, fi)
		}
	}

	sort.Sort(utils.ByModtime(fileInfos))

	var fileInfoOld []os.FileInfo

	logMsg := strings.Builder{}
	_, _ = fmt.Fprintf(&logMsg, "现有备份文件 %d 个, 清理模式为 ", len(fileInfos)) //nolint:gosec

	switch dm.BackupCleanStrategy {
	case BackupCleanStrategyByCount:
		_, _ = fmt.Fprintf(&logMsg, "保留一定数量(%d)", dm.BackupCleanKeepCount)
		if len(fileInfos) > dm.BackupCleanKeepCount {
			fileInfoOld = fileInfos[:len(fileInfos)-dm.BackupCleanKeepCount]
		}
	case BackupCleanStrategyByTime:
		threshold := time.Now().Add(-dm.BackupCleanKeepDur)
		_, _ = fmt.Fprintf(&logMsg, "保留一定时间(%v, %s)", dm.BackupCleanKeepDur, threshold.Format(time.DateTime))
		idx, _ := sort.Find(len(fileInfos), func(i int) int {
			return threshold.Compare(fileInfos[i].ModTime())
		})
		fileInfoOld = fileInfos[:idx]
	default:
		// no-op
	}

	_, _ = fmt.Fprintf(&logMsg, ", 有以下 %d 个将要被删除", len(fileInfoOld)) //nolint:gosec

	errDel := []string{}
	for i, fi := range fileInfoOld {
		_, _ = fmt.Fprintf(&logMsg, "\n%d. %s", i+1, fi.Name())     //nolint:gosec
		errDelete := os.Remove(filepath.Join(BackupDir, fi.Name())) //nolint:gosec
		if errDelete != nil {
			errDel = append(errDel, errDelete.Error())
		}
	}

	log.Info(logMsg.String())

	if len(errDel) > 0 {
		return errors.New("error(s) occured when deleting files:\n" + strings.Join(errDel, "\n"))
	}
	return nil
}
