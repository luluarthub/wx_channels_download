package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/rs/zerolog"
	"gorm.io/gorm"

	"wx_channel/internal/database"
	"wx_channel/internal/database/model"
	"wx_channel/internal/services"
	"wx_channel/pkg/hermes"
	"wx_channel/pkg/hermes/protocol"
)

func TestCreateByURLHandlerDownloadsVerifiedHTTPBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("URL-handler-download-integrity\x00\x01\x02"), 32000)
	want_hash := sha256.Sum256(payload)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "fixture.bin", time.Unix(1, 0), bytes.NewReader(payload))
	}))
	defer source.Close()
	for _, operation := range []string{"start", "legacy_retry", "legacy_resume", "legacy_batch_start"} {
		t.Run(operation, func(t *testing.T) {
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
			if err := db.AutoMigrate(&model.DownloadTask{}, &model.DownloadResource{}, &model.DownloadEndpoint{}, &model.DownloadSegment{}, &model.DownloadConnection{}); err != nil {
				t.Fatal(err)
			}
			engine := hermes.New(hermes.HermesNewConfig{Store: database.NewDBTaskStore(db, nil), Config: hermes.HermesEngineConfig{BasePath: dir}})
			engine.RegisterProtocol(protocol.NewHTTPDriver())
			t.Cleanup(engine.PauseAllTask)
			service := services.NewDownloadTaskService(db, nil, engine, nil, dir, dir, nil)
			logger := zerolog.Nop()
			client := &APIClient{db: db, logger: &logger, download_task_service: service}
			router := gin.New()
			router.POST("/create", client.handle_create_download_task_by_url)
			router.POST("/prepare", client.handle_prepare_download_task_by_url)
			body, err := json.Marshal(CreateDownloadTaskByURLRequest{Objects: []CreateDownloadTaskByURLBody{{URL: source.URL + "/fixture.bin", Filename: "download-output", AutoStart: func() *bool { value := false; return &value }()}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/prepare", "/create"} {
				request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
				}
				var count int64
				if err := db.Model(&model.DownloadTask{}).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if (path == "/prepare" && count != 0) || (path == "/create" && count != 1) {
					t.Fatalf("%s count=%d body=%s", path, count, recorder.Body.String())
				}
			}
			var task model.DownloadTask
			var resource model.DownloadResource
			if err := db.First(&task).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&resource).Error; err != nil {
				t.Fatal(err)
			}
			if resource.UniqueID == "" || task.Status != model.TaskStatusWaiting {
				t.Fatalf("non-executable created resource=%+v task=%+v", resource, task)
			}
			created_id := resource.UniqueID
			if operation != "start" {
				if err := db.Model(&resource).Update("unique_id", "").Error; err != nil {
					t.Fatal(err)
				}
				status := model.TaskStatusFailed
				if operation == "legacy_resume" {
					status = model.TaskStatusPaused
				}
				if err := db.Model(&task).Update("status", status).Error; err != nil {
					t.Fatal(err)
				}
			}
			switch operation {
			case "legacy_retry":
				_, err = service.RetryTask(task.Id)
			case "legacy_resume":
				_, err = service.ResumeTask(task.Id)
			case "legacy_batch_start":
				_, _, err = service.StartAllTasks("failed")
			default:
				_, err = service.StartTask(task.Id)
			}
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if err := db.First(&task, task.Id).Error; err != nil {
					t.Fatal(err)
				}
				if task.Status == model.TaskStatusFinished {
					break
				}
				if task.Status == model.TaskStatusFailed || time.Now().After(deadline) {
					t.Fatalf("download status=%d error=%s", task.Status, task.ErrorMessage)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := db.First(&resource, resource.Id).Error; err != nil {
				t.Fatal(err)
			}
			if resource.UniqueID != created_id {
				t.Fatalf("identity changed across retry: got=%q want=%q", resource.UniqueID, created_id)
			}
			data, err := os.ReadFile(filepath.Join(resource.DownloadDir, resource.Name))
			if err != nil {
				t.Fatal(err)
			}
			if got := sha256.Sum256(data); got != want_hash {
				t.Fatalf("download hash mismatch: got=%x want=%x", got, want_hash)
			}
		})
	}
}
