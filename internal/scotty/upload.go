package scotty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	UploadEndpoint = "https://push.clients6.google.com/upload/"
	PushID         = "feeds/mcudyrk2a4khkz"
	TenantID       = "bard-storage"
)

// FileSource enum (assistant.mobile.robin.FileSource)
const (
	FileSourceToolGenerated   int32 = 2  // view_file 첨부
	FileSourceRobinAutomation int32 = 17 // 0x11: ui_control 스크린샷 첨부
)

// FileType enum (assistant.mobile.robin.FileType)
const (
	FileTypeUnspecified int32 = 0
	FileTypeImage       int32 = 1
	FileTypeVideo       int32 = 2
	FileTypeText        int32 = 3
	FileTypeAudio       int32 = 4
	FileTypeJSON        int32 = 5
	FileTypeSheet       int32 = 6
	FileTypeExcel       int32 = 7
	FileTypeGDoc        int32 = 8
	FileTypeZip         int32 = 9
	FileTypeWord        int32 = 10
	FileTypePDF         int32 = 11
	FileTypePowerPoint  int32 = 12
	FileTypeGSlide      int32 = 13
	FileTypeODT         int32 = 14
	FileTypeHWP         int32 = 15
	FileTypeCode        int32 = 16
)

// BlobAttachment는 UserQueryAttachment.Blob 구조체와 매핑된다.
type BlobAttachment struct {
	BlobID     string
	FileType   int32
	MimeType   string
	FileSource int32
	Name       string
}

// Uploader는 FileUploadServiceImpl과 동일한 Scotty 업로드 프로토콜을 수행한다.
type Uploader struct {
	httpClient *http.Client
}

func NewUploader() *Uploader {
	return &Uploader{
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// Upload는 파일 바이트를 Scotty 서버에 업로드하고 blob_id를 반환한다.
func (u *Uploader) Upload(ctx context.Context, accessToken, fileName, mimeType string, data []byte, fileSource int32) (*BlobAttachment, error) {
	if mimeType == "" {
		mimeType = DetectMimeType(fileName, data)
	}
	if fileName == "" {
		fileName = "file"
	}

	// 1단계: Resumable 업로드 세션 시작
	startReq, err := http.NewRequestWithContext(ctx, http.MethodPost, UploadEndpoint, strings.NewReader("File name: "+fileName))
	if err != nil {
		return nil, err
	}
	startReq.Header.Set("Authorization", "Bearer "+accessToken)
	startReq.Header.Set("Push-ID", PushID)
	startReq.Header.Set("X-Tenant-Id", TenantID)
	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.Itoa(len(data)))
	startReq.Header.Set("X-Goog-Upload-Header-Content-Type", mimeType)
	startReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")

	startResp, err := u.httpClient.Do(startReq)
	if err != nil {
		return nil, fmt.Errorf("Scotty 업로드 세션 시작 실패: %w", err)
	}
	defer startResp.Body.Close()

	uploadURL := startResp.Header.Get("X-Goog-Upload-URL")
	if startResp.StatusCode != http.StatusOK || uploadURL == "" {
		body, _ := io.ReadAll(startResp.Body)
		return nil, fmt.Errorf("Scotty 업로드 URL 발급 실패 (HTTP %d): %s", startResp.StatusCode, string(body))
	}

	// 2단계: 바이트 업로드 및 finalize
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	upReq.Header.Set("Authorization", "Bearer "+accessToken)
	upReq.Header.Set("Push-ID", PushID)
	upReq.Header.Set("X-Tenant-Id", TenantID)
	upReq.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	upReq.Header.Set("X-Goog-Upload-Offset", "0")
	upReq.Header.Set("Content-Type", mimeType)

	upResp, err := u.httpClient.Do(upReq)
	if err != nil {
		return nil, fmt.Errorf("Scotty 바이트 전송 실패: %w", err)
	}
	defer upResp.Body.Close()
	respBytes, err := io.ReadAll(upResp.Body)
	if err != nil {
		return nil, err
	}
	if upResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Scotty 업로드 finalize 실패 (HTTP %d): %s", upResp.StatusCode, string(respBytes))
	}

	blobID := strings.TrimSpace(string(respBytes))
	if blobID == "" {
		return nil, fmt.Errorf("Scotty 응답에서 빈 blob_id 수신")
	}

	return &BlobAttachment{
		BlobID:     blobID,
		FileType:   ClassifyFileType(mimeType, fileName),
		MimeType:   mimeType,
		FileSource: fileSource,
		Name:       fileName,
	}, nil
}

func DetectMimeType(fileName string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext != "" {
		if m := mime.TypeByExtension(ext); m != "" {
			return strings.Split(m, ";")[0]
		}
	}
	if len(data) > 0 {
		return strings.Split(http.DetectContentType(data), ";")[0]
	}
	return "application/octet-stream"
}

func ClassifyFileType(mimeType, fileName string) int32 {
	m := strings.ToLower(mimeType)
	ext := strings.ToLower(filepath.Ext(fileName))
	switch {
	case strings.HasPrefix(m, "image/"):
		return FileTypeImage
	case strings.HasPrefix(m, "video/"):
		return FileTypeVideo
	case strings.HasPrefix(m, "audio/"):
		return FileTypeAudio
	case m == "application/pdf" || ext == ".pdf":
		return FileTypePDF
	case m == "application/json" || ext == ".json":
		return FileTypeJSON
	case m == "application/zip" || ext == ".zip":
		return FileTypeZip
	case ext == ".xlsx" || ext == ".xls" || ext == ".csv":
		return FileTypeExcel
	case ext == ".docx" || ext == ".doc":
		return FileTypeWord
	case ext == ".pptx" || ext == ".ppt":
		return FileTypePowerPoint
	case ext == ".odt":
		return FileTypeODT
	case ext == ".hwp" || ext == ".hwpx":
		return FileTypeHWP
	case strings.HasPrefix(m, "text/"):
		return FileTypeText
	default:
		return FileTypeUnspecified
	}
}
