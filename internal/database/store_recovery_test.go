package database

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"wx_channel/internal/database/model"
)

func recovery_test_db(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "recovery.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sql_db, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sql_db.Close() })
	if err := db.AutoMigrate(&model.DownloadTask{}, &model.DownloadResource{}, &model.DownloadEndpoint{}, &model.DownloadConnection{}, &model.DownloadSegment{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRecoverInterruptedTasksPreservesResumableState(t *testing.T) {
	db := recovery_test_db(t)
	deleted_at := int64(123)
	statuses := []int{model.TaskStatusPreparing, model.TaskStatusDownloading, model.TaskStatusMerging, model.TaskStatusFinished, model.TaskStatusWaiting, model.TaskStatusPaused, model.TaskStatusFailed, model.TaskStatusCancelled, model.TaskStatusDownloading}
	for i, status := range statuses {
		id := i + 1
		task := model.DownloadTask{Id: id, Name: fmt.Sprintf("task%d", id), Status: status, Timestamps: model.Timestamps{CreatedAt: 1}}
		if id == len(statuses) {
			task.DeletedAt = &deleted_at
		}
		resource := model.DownloadResource{Id: id, TaskId: &id, Name: "video.tmp", Status: 1, Downloaded: 42, Size: 100, Speed: 99}
		if id == 3 {
			resource.Type = model.ResourceTypeStream
		}
		if id == 4 {
			resource.Status, resource.Downloaded = 2, 100
		}
		endpoint := model.DownloadEndpoint{Id: id, ResourceId: id, Protocol: "http", URL: "http://localhost/video", Status: 1}
		connection := model.DownloadConnection{Id: id, EndpointId: id, Status: 1, Speed: 99, Bytes: 42}
		segment := model.DownloadSegment{Id: id, ResourceId: id, Index: 0, Status: 1, Downloaded: 42, Size: 100, OffsetEnd: 99}
		for _, row := range []any{&task, &resource, &endpoint, &connection, &segment} {
			if err := db.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	store := NewDBTaskStore(db, nil)
	count, err := store.RecoverInterruptedTasks()
	if err != nil || count != 3 {
		t.Fatalf("recovered=%d err=%v, want 3", count, err)
	}
	for i, original_status := range statuses {
		id := i + 1
		var task model.DownloadTask
		var resource model.DownloadResource
		var endpoint model.DownloadEndpoint
		var connection model.DownloadConnection
		var segment model.DownloadSegment
		for _, row := range []any{&task, &resource, &endpoint, &connection, &segment} {
			if err := db.Unscoped().First(row, id).Error; err != nil {
				t.Fatal(err)
			}
		}
		want_status := original_status
		if id <= 3 {
			want_status = model.TaskStatusPaused
		}
		if task.Status != want_status {
			t.Errorf("task %d status=%d want=%d", id, task.Status, want_status)
		}
		if id < len(statuses) {
			if resource.Speed != 0 || endpoint.Status != 0 || connection.Status != 0 || connection.Speed != 0 {
				t.Errorf("task %d retains stale activity", id)
			}
		} else if resource.Speed != 99 || endpoint.Status != 1 || connection.Status != 1 || connection.Speed != 99 {
			t.Error("deleted task graph was modified")
		}
		want_downloaded, want_resource_status := int64(42), 1
		if id == 4 {
			want_downloaded, want_resource_status = 100, 2
		}
		if resource.Downloaded != want_downloaded || resource.Status != want_resource_status || resource.Name != "video.tmp" || connection.Bytes != 42 || segment.Downloaded != 42 || segment.Status != 1 || segment.OffsetEnd != 99 {
			t.Errorf("task %d lost durable progress or filename", id)
		}
	}
	if count, err := store.RecoverInterruptedTasks(); err != nil || count != 0 {
		t.Fatalf("recovery should be idempotent: count=%d err=%v", count, err)
	}
}

func TestRecoverInterruptedTasksRollsBackOnFailure(t *testing.T) {
	db := recovery_test_db(t)
	task := model.DownloadTask{Id: 1, Name: "task", Status: model.TaskStatusDownloading, Timestamps: model.Timestamps{CreatedAt: 1}}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&model.DownloadConnection{}); err != nil {
		t.Fatal(err)
	}
	count, err := NewDBTaskStore(db, nil).RecoverInterruptedTasks()
	if err == nil || count != 0 {
		t.Fatalf("failed recovery returned count=%d err=%v", count, err)
	}
	if err := db.First(&task, 1).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != model.TaskStatusDownloading {
		t.Fatal("partial recovery must roll back task state")
	}
}
