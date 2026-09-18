package services

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"wx_channel/internal/database"
	"wx_channel/internal/database/model"
	"wx_channel/pkg/hermes"
)

// Simulate a third-party driver that fails to honor cancellation promptly.
// No external server or user's download database is used by these tests.
type blocked_download_driver struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blocked_download_driver) Protocols() []string { return []string{"blocked"} }
func (d *blocked_download_driver) Prepare(ctx context.Context, _ hermes.Endpoint) (hermes.PreparedResource, error) {
	d.once.Do(func() { close(d.entered) })
	<-d.release
	return hermes.PreparedResource{}, ctx.Err()
}
func (d *blocked_download_driver) Open(context.Context, hermes.Endpoint, hermes.ReadRequest) (io.ReadCloser, error) {
	return nil, errors.New("unexpected open")
}

func TestCancellationTimeoutPreservesTaskGraphAndFiles(t *testing.T) {
	for _, operation := range []string{"pause", "pause_all", "delete", "delete_with_files", "clear"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
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
			if err := db.AutoMigrate(&model.DownloadTask{}, &model.DownloadResource{}, &model.DownloadEndpoint{}, &model.DownloadConnection{}, &model.DownloadSegment{}); err != nil {
				t.Fatal(err)
			}
			task_id := 1
			task := model.DownloadTask{Id: task_id, Name: "video", Status: model.TaskStatusWaiting, Timestamps: model.Timestamps{CreatedAt: 1}}
			resource := model.DownloadResource{Id: 1, TaskId: &task_id, Name: "video.tmp", DownloadDir: dir, Type: model.ResourceTypeFile, Downloaded: 4, Size: 10}
			endpoint := model.DownloadEndpoint{Id: 1, ResourceId: 1, Protocol: "blocked", URL: "blocked://test/video", Enabled: 1}
			segment := model.DownloadSegment{Id: 1, ResourceId: 1, Size: 10, Downloaded: 4, OffsetEnd: 9, Status: 1}
			for _, row := range []any{&task, &resource, &endpoint, &segment} {
				if err := db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			part := filepath.Join(dir, "video.tmp.part")
			if err := os.WriteFile(part, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			driver := &blocked_download_driver{entered: make(chan struct{}), release: make(chan struct{})}
			engine := hermes.New(hermes.HermesNewConfig{Store: database.NewDBTaskStore(db, nil)})
			engine.RegisterProtocol(driver)
			t.Cleanup(func() {
				close(driver.release)
				engine.PauseAllTask()
			})
			service := NewDownloadTaskService(db, nil, engine, nil, dir, dir, nil)
			if err := engine.StartTask(task_id); err != nil {
				t.Fatal(err)
			}
			select {
			case <-driver.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("download driver did not start")
			}
			switch operation {
			case "pause":
				_, _, err = service.PauseTask(task_id)
			case "pause_all":
				_, _, err = service.PauseAllTasks("running")
			case "delete":
				_, err = service.DeleteTask(task_id)
			case "delete_with_files":
				err = service.DeleteTaskWithFiles(task_id, true)
			case "clear":
				if err := db.Model(&task).Update("status", model.TaskStatusCancelled).Error; err != nil {
					t.Fatal(err)
				}
				_, err = service.ClearTasks(true)
			}
			if err == nil || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("operation must surface timeout, got %v", err)
			}
			for _, row := range []any{&task, &resource, &endpoint, &segment} {
				if err := db.Unscoped().First(row, 1).Error; err != nil {
					t.Fatal(err)
				}
			}
			if task.DeletedAt != nil || resource.DeletedAt != nil || endpoint.DeletedAt != nil || segment.DeletedAt != nil {
				t.Fatal("timeout must preserve the entire task graph")
			}
			if operation != "clear" && task.Status != model.TaskStatusDownloading {
				t.Fatalf("timeout must not report successful pause or deletion: status=%d", task.Status)
			}
			if segment.Downloaded != 4 {
				t.Fatal("timeout must retain resumable offsets")
			}
			if data, err := os.ReadFile(part); err != nil || string(data) != "keep" {
				t.Fatalf("timeout removed or changed partial file: data=%q err=%v", data, err)
			}
		})
	}
}
