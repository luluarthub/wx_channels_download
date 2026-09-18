package services

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"wx_channel/internal/database/model"
)

func TestURLTaskIdentitiesAreIsolatedAndLegacyRepairIsScoped(t *testing.T) {
	dir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "tasks.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sql_db, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sql_db.Close() })
	if err := db.AutoMigrate(&model.DownloadTask{}, &model.DownloadResource{}, &model.DownloadEndpoint{}, &model.DownloadSegment{}); err != nil {
		t.Fatal(err)
	}
	service := NewDownloadTaskService(db, nil, nil, nil, dir, dir, nil)
	no_start := false
	body := CreateDownloadTaskByURLBody{URL: "https://example.invalid/same.bin?signature=preserved", Filename: "same", AutoStart: &no_start}
	first, err := service.CreateTaskByURL(body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateTaskByURL(body)
	if err != nil {
		t.Fatal(err)
	}
	if first.Resource.UniqueID == "" || first.Resource.UniqueID == second.Resource.UniqueID {
		t.Fatal("same-URL tasks must have separate temporary paths")
	}
	if first.Endpoint.URL != body.URL || second.Endpoint.URL != body.URL {
		t.Fatal("identity generation modified signed URL")
	}
	for _, scenario := range []struct {
		name, platform, identity, endpoint string
		deleted, want_repair               bool
	}{
		{name: "legacy URL", endpoint: body.URL, want_repair: true},
		{name: "existing URL identity", identity: "existing-canonical-id", endpoint: body.URL},
		{name: "platform empty identity", platform: "wxchannels", endpoint: body.URL},
		{name: "platform identity", platform: "wxchannels", identity: "wx-video-original", endpoint: body.URL},
		{name: "unrelated endpoint", endpoint: "https://example.invalid/other.bin"},
		{name: "deleted resource", endpoint: body.URL, deleted: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			config_json, _ := json.Marshal(map[string]string{"url": body.URL})
			task := model.DownloadTask{Name: scenario.name, PlatformId: scenario.platform, ConfigJSON: string(config_json), Timestamps: model.Timestamps{CreatedAt: 1}}
			if err := db.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			resource := model.DownloadResource{TaskId: &task.Id, Name: "canonical.tmp", UniqueID: scenario.identity, Downloaded: 17, Status: 1}
			if scenario.deleted {
				deleted := int64(100)
				resource.DeletedAt = &deleted
			}
			if err := db.Create(&resource).Error; err != nil {
				t.Fatal(err)
			}
			endpoint := model.DownloadEndpoint{ResourceId: resource.Id, URL: scenario.endpoint, Protocol: "HTTPS", Enabled: 1}
			if err := db.Create(&endpoint).Error; err != nil {
				t.Fatal(err)
			}
			segment := model.DownloadSegment{ResourceId: resource.Id, OffsetStart: 0, OffsetEnd: 30, Downloaded: 17, Size: 31, Status: 1}
			if err := db.Create(&segment).Error; err != nil {
				t.Fatal(err)
			}
			if err := service.repair_url_resource_ids(task.Id); err != nil {
				t.Fatal(err)
			}
			if err := db.Unscoped().First(&resource, resource.Id).Error; err != nil {
				t.Fatal(err)
			}
			want_id := scenario.identity
			if scenario.want_repair {
				want_id = url_download_resource_id(task.Id, body.URL)
			}
			if resource.UniqueID != want_id || resource.Name != "canonical.tmp" || resource.Downloaded != 17 || resource.Status != 1 {
				t.Fatalf("unexpected legacy change: %+v", resource)
			}
			if err := db.First(&segment, segment.Id).Error; err != nil {
				t.Fatal(err)
			}
			if segment.Downloaded != 17 || segment.OffsetEnd != 30 {
				t.Fatal("repair damaged persisted segment offsets")
			}
		})
	}
}
