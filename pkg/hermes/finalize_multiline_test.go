package hermes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultilineTitleFinalizesAsWindowsFilename(t *testing.T) {
	job := &TaskJob{ID: 1, DownloadDir: t.TempDir(), Resources: []ResourceJob{{ID: 1, UniqueID: "video", Name: "Marriage,\nunderstand people,\r\nunderstand men\t", Kind: "video/mp4", Type: "file"}}}
	job.Resources[0].FilePath = filepath.Join(job.DownloadDir, "video")
	d := New(HermesNewConfig{})
	name := BuildFinalResourceName(FinalResourceNameInput{ResourceName: job.Resources[0].Name, ResourceKind: "video/mp4"}).Name
	if strings.ContainsAny(name, "\r\n\t") || !strings.HasSuffix(name, ".mp4") {
		t.Fatalf("invalid final name %q", name)
	}
	if err := os.WriteFile(job.Resources[0].FilePath, []byte("downloaded video"), 0600); err != nil {
		t.Fatal(err)
	}
	d.finalize_resource_filenames(job)
	got := job.Resources[0].Name
	if got != name {
		t.Fatalf("finalized name: got %q, want %q", got, name)
	}
	if data, err := os.ReadFile(job.Resources[0].FilePath); err != nil || string(data) != "downloaded video" {
		t.Fatalf("finalized file not readable: data=%q err=%v", data, err)
	}
}
