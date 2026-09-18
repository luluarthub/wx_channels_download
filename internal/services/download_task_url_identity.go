package services

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"wx_channel/internal/database/model"
)

// URL tasks can intentionally download the same URL more than once. Include
// the task identity so concurrent tasks never write the same temporary file.
// Persist the result once so retries and resumes retain their original path.
func url_download_resource_id(task_id int, source_url string) string {
	digest := sha256.Sum256([]byte(source_url))
	return fmt.Sprintf("url_%d_%x", task_id, digest[:16])
}

func (s *DownloadTaskService) start_existing_download_task(task_id int) error {
	if s.downloader == nil {
		return fmt.Errorf("下载器未初始化")
	}
	if err := s.repair_url_resource_ids(task_id); err != nil {
		return fmt.Errorf("修复 URL 下载资源标识失败: %w", err)
	}
	return s.downloader.StartTask(task_id)
}

// Older create-by-URL handlers persisted an empty resource unique_id, which
// newer Hermes cannot execute. Repair only this identifiable legacy graph;
// platform-provided identities, existing names, and segment offsets are kept.
func (s *DownloadTaskService) repair_url_resource_ids(task_id int) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var task model.DownloadTask
		if err := tx.Where("id = ? AND deleted_at IS NULL", task_id).First(&task).Error; err != nil {
			return err
		}
		if strings.TrimSpace(task.PlatformId) != "" {
			return nil
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(task.ConfigJSON), &cfg); err != nil {
			return nil // Leave invalid/non-URL configs to the normal loader.
		}
		source_url, _ := cfg["url"].(string)
		if strings.TrimSpace(source_url) == "" {
			return nil
		}
		endpoints := tx.Model(&model.DownloadEndpoint{}).Select("resource_id").
			Where("url = ? AND enabled = 1 AND deleted_at IS NULL", source_url)
		return tx.Model(&model.DownloadResource{}).
			Where("task_id = ? AND deleted_at IS NULL AND (unique_id IS NULL OR TRIM(unique_id) = '')", task_id).
			Where("id IN (?)", endpoints).
			Updates(map[string]any{
				"unique_id":  url_download_resource_id(task_id, source_url),
				"updated_at": time.Now().UnixMilli(),
			}).Error
	})
}
