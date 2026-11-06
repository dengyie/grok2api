package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestGenerateVideoFirstFrame(t *testing.T) {
	const image = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	tests := []struct {
		name          string
		image         string
		references    []string
		maxBytes      int64
		uploadStatus  int
		uploadBody    string
		wantRetryable bool
		cancelUpload  bool
		wantUploads   int32
		wantVideo     int32
		wantDuration  int
		basic         bool
	}{
		{name: "metadata first frame", image: image, uploadBody: `{"fileMetadata":{"fileMetadataId":"metadata-1","fileId":"wrong-file"},"uploadId":"wrong-upload"}`, wantUploads: 1, wantVideo: 1, wantDuration: 8},
		{name: "basic duration cap", image: image, uploadBody: `{"fileMetadata":{"fileMetadataId":"metadata-1"}}`, wantUploads: 1, wantVideo: 1, wantDuration: 6, basic: true},
		{name: "invalid image", image: "data:image/png;base64,bm90LWFuLWltYWdl"},
		{name: "oversized image", image: image, maxBytes: 8},
		{name: "reference list rejected", image: image, references: []string{image}},
		{name: "upload rejected", image: image, uploadStatus: 503, uploadBody: `{"error":{"message":"upload unavailable"}}`, wantUploads: 1},
		{name: "upload challenge", image: image, uploadStatus: 403, uploadBody: `<!doctype html><title>Just a moment...</title>`, wantUploads: 1, wantRetryable: true},
		{name: "upload moderation", image: image, uploadStatus: 403, uploadBody: `{"error":{"code":"content-moderated","message":"rejected"}}`, wantUploads: 1},
		{name: "file ID is not metadata", image: image, uploadBody: `{"fileMetadata":{"fileId":"wrong-file"}}`, wantUploads: 1},
		{name: "upload ID is not metadata", image: image, uploadBody: `{"uploadId":"wrong-upload"}`, wantUploads: 1},
		{name: "cancel during upload", image: image, uploadBody: `{"fileMetadata":{"fileMetadataId":"metadata-1"}}`, cancelUpload: true, wantUploads: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var uploads, videos atomic.Int32
			var baseURL string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Referer") != baseURL+"/imagine" {
					t.Errorf("referer = %q", r.Header.Get("Referer"))
				}
				cookie, err := r.Cookie("sso")
				if err != nil || cookie.Value != "test-sso" {
					t.Errorf("upload and generation must use the same SSO identity")
				}
				switch r.URL.Path {
				case "/http/upload-file-v2/direct":
					uploads.Add(1)
					reader, err := r.MultipartReader()
					if err != nil {
						t.Errorf("multipart upload: %v", err)
						return
					}
					fields := map[string]string{}
					for {
						part, err := reader.NextPart()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Errorf("upload part: %v", err)
							return
						}
						data, _ := io.ReadAll(part)
						fields[part.FormName()] = string(data)
						if part.FormName() == "file" && part.Header.Get("Content-Type") != "image/png" {
							t.Errorf("file type = %q", part.Header.Get("Content-Type"))
						}
					}
					decoded, _ := base64.StdEncoding.DecodeString(image[len("data:image/png;base64,"):])
					if fields["file_source"] != imagineSelfUploadSource || fields["file"] != string(decoded) {
						t.Errorf("first-frame bytes/source not preserved")
					}
					if test.cancelUpload {
						cancel()
					}
					w.Header().Set("Content-Type", "application/json")
					if test.uploadStatus != 0 {
						w.WriteHeader(test.uploadStatus)
					}
					_, _ = io.WriteString(w, test.uploadBody)
				case "/rest/app-chat/conversations/new":
					videos.Add(1)
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("video JSON: %v", err)
						return
					}
					mediaInput, _ := payload["mediaGenInput"].(map[string]any)
					want := map[string]any{"imageToVideo": map[string]any{
						"prompt": "animate this cat", "aspectRatio": "16:9", "duration": float64(test.wantDuration),
						"resolutionName": "720p", "skipAudio": true, "mode": "custom", "inputAssets": []any{"metadata-1"},
					}}
					if !reflect.DeepEqual(mediaInput, want) || len(payload) != 8 || payload["message"] != "animate this cat --mode=custom" {
						t.Errorf("first-frame payload = %#v", payload)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post_1","videoUrl":"/videos/final.mp4"}}}}`+"\n\n")
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			baseURL = server.URL
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			token, err := cipher.Encrypt("test-sso")
			if err != nil {
				t.Fatal(err)
			}
			adapter := NewAdapter(Config{BaseURL: baseURL, StatsigMode: "manual", StatsigManualValue: "test", VideoTimeoutSeconds: 5, MaxInputImageBytes: test.maxBytes, FreeVideoDurationCap: 6}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
			credential := account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: token}
			if test.basic {
				credential.WebTier = account.WebTierBasic
			}
			progress := 0
			result, err := adapter.GenerateVideo(ctx, provider.VideoRequest{Credential: credential, Prompt: "animate this cat", ImageURL: test.image, ReferenceURLs: test.references, Duration: 8, AspectRatio: "16:9", Resolution: "720p", Progress: func(value int) { progress = value }})
			if test.wantVideo == 1 {
				if err != nil || result.URL != "https://assets.grok.com/videos/final.mp4" || progress != 100 {
					t.Errorf("result=%#v progress=%d err=%v", result, progress, err)
				}
			} else {
				if err == nil {
					t.Fatal("preparation failure unexpectedly succeeded")
				}
				if stage, ok := provider.VideoErrorStage(err); !ok || stage != provider.VideoStagePrepare {
					t.Errorf("preparation stage = %v: %v", stage, err)
				}
				var retryable interface{ VideoPreparationRetryable() bool }
				gotRetryable := errors.As(err, &retryable) && retryable.VideoPreparationRetryable()
				if gotRetryable != test.wantRetryable {
					t.Errorf("retryable=%v, want %v", gotRetryable, test.wantRetryable)
				}
				if test.cancelUpload && !errors.Is(err, context.Canceled) {
					t.Errorf("cancellation lost: %v", err)
				}
			}
			if uploads.Load() != test.wantUploads || videos.Load() != test.wantVideo {
				t.Errorf("uploads=%d videos=%d; want %d/%d", uploads.Load(), videos.Load(), test.wantUploads, test.wantVideo)
			}
		})
	}
}
