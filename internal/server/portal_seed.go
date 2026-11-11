// 内置示例认证模板播种：按 code 补齐缺失项（已存在的跳过，不覆盖用户配置），
// 使升级后新增的内置模板也能自动入库；示例模板与上传模板无差别，可复制/删除。
package server

import (
	"log/slog"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"wrthub-ui/internal/portal"
	"wrthub-ui/internal/store"
)

// seedPortalExamples 播种全部内置示例模板：已存在的 code 跳过，缺失的补齐。
// 单个示例失败仅记日志并清理，不阻断启动。
func seedPortalExamples(db *gorm.DB, ps *portal.Store, log *slog.Logger) error {
	// 历史数据兼容：早期播种的系统模板 created_by=system，统一标记为系统模板（不可删除）。
	if err := db.Model(&store.PortalTemplate{}).
		Where("created_by = ? AND is_system = ?", "system", false).
		Update("is_system", true).Error; err != nil {
		log.Warn("mark system portal templates failed", "err", err)
	}

	for _, ex := range portal.ExampleTemplates() {
		var cnt int64
		if err := db.Model(&store.PortalTemplate{}).Where("code = ?", ex.Code).Count(&cnt).Error; err != nil {
			log.Error("check built-in portal template failed", "code", ex.Code, "err", err)
			continue
		}
		if cnt > 0 {
			continue
		}
		// 目录已存在（如数据库重建）时保留现场文件，仅补数据库记录。
		if !ps.HasEntry(ex.Code, ex.EntryPage) {
			if err := ps.WriteDir(ex.Code, ex.FS()); err != nil {
				_ = ps.Remove(ex.Code)
				log.Error("seed portal example failed", "code", ex.Code, "err", err)
				continue
			}
		}
		files, ferr := ps.Files(ex.Code)
		if ferr != nil {
			log.Error("scan seeded portal example failed", "code", ex.Code, "err", ferr)
		}
		var total int64
		for _, f := range files {
			total += f.Size
		}
		t := &store.PortalTemplate{
			ID:          uuid.NewString(),
			Code:        ex.Code,
			Name:        ex.Name,
			Description: ex.Description,
			EntryPage:   ex.EntryPage,
			Status:      "enabled",
			FileCount:   len(files),
			TotalSize:   total,
			HasSchema:   ps.HasSchemaFile(ex.Code),
			CreatedBy:   "system",
			IsSystem:    true,
		}
		if err := db.Create(t).Error; err != nil {
			_ = ps.Remove(ex.Code)
			log.Error("seed portal example db failed", "code", ex.Code, "err", err)
			continue
		}
		log.Info("seeded built-in portal template", "code", ex.Code, "name", ex.Name)
	}
	return nil
}
