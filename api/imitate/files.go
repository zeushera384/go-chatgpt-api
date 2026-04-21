package imitate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/gin-gonic/gin"
	"github.com/leokwsw/go-chatgpt-api/api"
	"github.com/leokwsw/go-chatgpt-api/api/chatgpt"
)

var fileIndexCache sync.Map

type createFileRequest struct {
	FileName               string `json:"file_name"`
	FileSize               int64  `json:"file_size"`
	UseCase                string `json:"use_case"`
	TimezoneOffsetMin      int    `json:"timezone_offset_min"`
	ResetRateLimits        bool   `json:"reset_rate_limits"`
	StoreInLibrary         bool   `json:"store_in_library"`
	LibraryPersistenceMode string `json:"library_persistence_mode"`
}

type createFileResponse struct {
	Status    string `json:"status"`
	UploadURL string `json:"upload_url"`
	FileID    string `json:"file_id"`
}

type processUploadRequest struct {
	FileID                 string                 `json:"file_id"`
	UseCase                string                 `json:"use_case"`
	IndexForRetrieval      bool                   `json:"index_for_retrieval"`
	FileName               string                 `json:"file_name"`
	LibraryPersistenceMode string                 `json:"library_persistence_mode"`
	Metadata               map[string]interface{} `json:"metadata"`
}

type processUploadEvent struct {
	FileID   string                 `json:"file_id"`
	Event    string                 `json:"event"`
	Message  string                 `json:"message"`
	Progress *float64               `json:"progress"`
	Extra    map[string]interface{} `json:"extra"`
}

type libraryItem struct {
	ID                string `json:"id"`
	FileID            string `json:"file_id"`
	FileName          string `json:"file_name"`
	MimeType          string `json:"mime_type"`
	FileSizeBytes     int64  `json:"file_size_bytes"`
	RecordCreationRaw string `json:"record_creation_time"`
}

type libraryResponse struct {
	Items []libraryItem `json:"items"`
}

type openAIFile struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

type openAIFileList struct {
	Object string       `json:"object"`
	Data   []openAIFile `json:"data"`
}

type openAIFileDelete struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

func CreateFile(c *gin.Context) {
	accessToken := resolveImitateAccessToken(c)
	if accessToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API KEY is missing or invalid"})
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file form field"})
		return
	}

	purpose := c.PostForm("purpose")
	if purpose == "" {
		purpose = "assistants"
	}

	src, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer src.Close()

	fileBytes, err := io.ReadAll(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	useCase := inferUseCase(fileHeader.Header.Get("Content-Type"), purpose)
	createResp, err := createBackendFile(c, accessToken, fileHeader.Filename, int64(len(fileBytes)), useCase)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	contentType := fileHeader.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(fileBytes)
	}
	if err = uploadToSignedURL(createResp.UploadURL, fileBytes, contentType); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	shouldIndex := !strings.HasPrefix(contentType, "image/")
	libraryID, libraryName, libraryMime, err := processUpload(c, accessToken, createResp.FileID, useCase, shouldIndex, fileHeader.Filename)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	fileIndexCache.Store(createResp.FileID, libraryID)

	filename := fileHeader.Filename
	if libraryName != "" {
		filename = libraryName
	}
	if contentType == "" {
		contentType = libraryMime
	}

	c.JSON(http.StatusOK, openAIFile{
		ID:        createResp.FileID,
		Object:    "file",
		Bytes:     int64(len(fileBytes)),
		CreatedAt: time.Now().Unix(),
		Filename:  filename,
		Purpose:   purpose,
	})
}

func ListFiles(c *gin.Context) {
	accessToken := resolveImitateAccessToken(c)
	if accessToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API KEY is missing or invalid"})
		return
	}

	items, err := listLibraryFiles(c, accessToken)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	resp := openAIFileList{
		Object: "list",
		Data:   make([]openAIFile, 0, len(items)),
	}
	for _, item := range items {
		resp.Data = append(resp.Data, mapLibraryItemToOpenAI(item))
	}
	c.JSON(http.StatusOK, resp)
}

func RetrieveFile(c *gin.Context) {
	accessToken := resolveImitateAccessToken(c)
	if accessToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API KEY is missing or invalid"})
		return
	}

	fileID := c.Param("id")
	item, err := findLibraryFile(c, accessToken, fileID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	c.JSON(http.StatusOK, mapLibraryItemToOpenAI(*item))
}

func DeleteFile(c *gin.Context) {
	fileID := c.Param("id")
	fileIndexCache.Delete(fileID)
	c.JSON(http.StatusOK, openAIFileDelete{
		ID:      fileID,
		Object:  "file",
		Deleted: true,
	})
}

func createBackendFile(c *gin.Context, accessToken string, filename string, fileSize int64, useCase string) (*createFileResponse, error) {
	reqBody := createFileRequest{
		FileName:               filename,
		FileSize:               fileSize,
		UseCase:                useCase,
		TimezoneOffsetMin:      -480,
		ResetRateLimits:        false,
		StoreInLibrary:         true,
		LibraryPersistenceMode: "opportunistic",
	}
	jsonBytes, _ := json.Marshal(reqBody)
	resp, err := doChatGPTJSONRequest(accessToken, http.MethodPost, chatgpt.ApiPrefix+"/files", bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, errors.New(string(body))
	}

	var createResp createFileResponse
	if err = json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, err
	}
	return &createResp, nil
}

func uploadToSignedURL(uploadURL string, fileBytes []byte, contentType string) error {
	req, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewBuffer(fileBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := api.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return errors.New(string(body))
	}
	return nil
}

func processUpload(c *gin.Context, accessToken string, fileID string, useCase string, indexForRetrieval bool, fileName string) (string, string, string, error) {
	reqBody := processUploadRequest{
		FileID:                 fileID,
		UseCase:                useCase,
		IndexForRetrieval:      indexForRetrieval,
		FileName:               fileName,
		LibraryPersistenceMode: "opportunistic",
		Metadata: map[string]interface{}{
			"store_in_library": true,
		},
	}
	jsonBytes, _ := json.Marshal(reqBody)
	resp, err := doChatGPTJSONRequest(accessToken, http.MethodPost, chatgpt.ApiPrefix+"/files/process_upload_stream", bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", "", errors.New(string(body))
	}

	var libraryID, libraryName, libraryMime string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt processUploadEvent
		if err = json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if evt.Event == "file.indexing.completed" && evt.Extra != nil {
			if v, ok := evt.Extra["metadata_object_id"].(string); ok {
				libraryID = v
			}
			if v, ok := evt.Extra["library_file_name"].(string); ok {
				libraryName = v
			}
			if v, ok := evt.Extra["mime_type"].(string); ok {
				libraryMime = v
			}
		}
	}
	if err = scanner.Err(); err != nil {
		return "", "", "", err
	}
	return libraryID, libraryName, libraryMime, nil
}

func listLibraryFiles(c *gin.Context, accessToken string) ([]libraryItem, error) {
	jsonBytes := []byte(`{"limit":50,"cursor":null}`)
	resp, err := doChatGPTJSONRequest(accessToken, http.MethodPost, chatgpt.ApiPrefix+"/files/library", bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, errors.New(string(body))
	}

	var library libraryResponse
	if err = json.NewDecoder(resp.Body).Decode(&library); err != nil {
		return nil, err
	}
	return library.Items, nil
}

func findLibraryFile(c *gin.Context, accessToken string, fileID string) (*libraryItem, error) {
	items, err := listLibraryFiles(c, accessToken)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.FileID == fileID {
			return &item, nil
		}
	}
	return nil, errors.New("not found")
}

func mapLibraryItemToOpenAI(item libraryItem) openAIFile {
	createdAt := time.Now().Unix()
	if item.RecordCreationRaw != "" {
		if t, err := time.Parse(time.RFC3339Nano, item.RecordCreationRaw); err == nil {
			createdAt = t.Unix()
		}
	}
	purpose := "assistants"
	if strings.HasPrefix(item.MimeType, "image/") {
		purpose = "vision"
	}
	return openAIFile{
		ID:        item.FileID,
		Object:    "file",
		Bytes:     item.FileSizeBytes,
		CreatedAt: createdAt,
		Filename:  item.FileName,
		Purpose:   purpose,
	}
}

func inferUseCase(contentType string, purpose string) string {
	if strings.HasPrefix(contentType, "image/") || strings.EqualFold(purpose, "vision") {
		return "multimodal"
	}
	return "my_files"
}

func doChatGPTJSONRequest(accessToken string, method string, endpoint string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", api.UserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", accessToken)
	req.Header.Set("Origin", api.ChatGPTApiUrlPrefix)
	req.Header.Set("Referer", api.ChatGPTApiUrlPrefix+"/")
	if api.PUID != "" {
		req.Header.Set("Cookie", "_puid="+api.PUID+";")
	}
	if api.OAIDID != "" {
		req.Header.Set("Cookie", req.Header.Get("Cookie")+"oai-did="+api.OAIDID+";")
		req.Header.Set("Oai-Device-Id", api.OAIDID)
	}
	req.Header.Set("Oai-Language", api.Language)
	return api.Client.Do(req)
}

func BuildAttachmentMetadataByFileID(c *gin.Context, accessToken string, fileID string) (map[string]interface{}, error) {
	item, err := findLibraryFile(c, accessToken, fileID)
	if err != nil {
		return nil, err
	}

	libraryID := item.ID
	if cached, ok := fileIndexCache.Load(fileID); ok {
		if cachedID, ok := cached.(string); ok && cachedID != "" {
			libraryID = cachedID
		}
	}

	return map[string]interface{}{
		"id":              fileID,
		"size":            item.FileSizeBytes,
		"name":            filepath.Base(item.FileName),
		"mime_type":       item.MimeType,
		"source":          "library",
		"library_file_id": libraryID,
		"is_big_paste":    false,
	}, nil
}
