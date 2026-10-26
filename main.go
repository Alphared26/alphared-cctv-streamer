package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed panduan.md
var panduanMD []byte

type AppSettings struct {
	YouTubeClientID     string `json:"youtube_client_id"`
	YouTubeClientSecret string `json:"youtube_client_secret"`
	YouTubeRefreshToken string `json:"youtube_refresh_token"`
	YouTubePlaylistID   string `json:"youtube_playlist_id"`
	LiveDurationMinutes int    `json:"live_duration_minutes"`
	RecordSegmentSec    int    `json:"record_segment_seconds"`
	DiskGuardMinGB      int    `json:"disk_guard_min_gb"`
	DiskRetentionDays   int    `json:"disk_retention_days"`
}

type SettingsStore struct {
	mu       sync.RWMutex
	filePath string
	settings AppSettings
}

func NewSettingsStore(filePath string) (*SettingsStore, error) {
	ss := &SettingsStore{
		filePath: filePath,
		settings: AppSettings{
			LiveDurationMinutes: 715,  // 11 jam 55 menit
			RecordSegmentSec:    3600, // 1 jam
			DiskGuardMinGB:      10,   // minimal 10 GB sisa HDD
			DiskRetentionDays:   7,
		},
	}
	if err := ss.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return ss, nil
}

func (ss *SettingsStore) load() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	data, err := os.ReadFile(ss.filePath)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	var loaded AppSettings
	if err := json.Unmarshal(data, &loaded); err != nil {
		return err
	}
	if loaded.LiveDurationMinutes <= 0 {
		loaded.LiveDurationMinutes = 715
	}
	if loaded.RecordSegmentSec <= 0 {
		loaded.RecordSegmentSec = 3600
	}
	if loaded.DiskGuardMinGB <= 0 {
		loaded.DiskGuardMinGB = 10
	}
	if loaded.DiskRetentionDays <= 0 {
		loaded.DiskRetentionDays = 7
	}
	ss.settings = loaded
	return nil
}

func (ss *SettingsStore) save() error {
	dir := filepath.Dir(ss.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ss.settings, "", "  ")
	if err != nil {
		return err
	}
	tmp := ss.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, ss.filePath)
}

func (ss *SettingsStore) Get() AppSettings {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.settings
}

func (ss *SettingsStore) Update(update func(s *AppSettings)) (AppSettings, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	update(&ss.settings)
	if ss.settings.LiveDurationMinutes <= 0 {
		ss.settings.LiveDurationMinutes = 715
	}
	if ss.settings.RecordSegmentSec <= 0 {
		ss.settings.RecordSegmentSec = 3600
	}
	if ss.settings.DiskGuardMinGB <= 0 {
		ss.settings.DiskGuardMinGB = 10
	}
	if ss.settings.DiskRetentionDays <= 0 {
		ss.settings.DiskRetentionDays = 7
	}
	return ss.settings, ss.save()
}

func getYouTubeAccessToken(clientID, clientSecret, refreshToken string) (string, error) {
	if clientID == "" || clientSecret == "" || refreshToken == "" {
		return "", fmt.Errorf("OAuth credentials not configured")
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	resp, err := http.PostForm("https://oauth2.googleapis.com/token", form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}
	if tokenResp.Error != "" {
		return "", fmt.Errorf("Google token error: %s (%s)", tokenResp.Error, tokenResp.ErrorDesc)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("Google returned empty access token")
	}
	return tokenResp.AccessToken, nil
}

func addVideoToYouTubePlaylist(accessToken, playlistID, videoID string) error {
	if playlistID == "" || videoID == "" {
		return fmt.Errorf("empty playlistID or videoID")
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// 1. Cek idempotensi: apakah videoID sudah ada di playlist?
	// Cegah duplikasi entri berkali-kali saat stream restart / watchdog trigger.
	checkURL := fmt.Sprintf("https://www.googleapis.com/youtube/v3/playlistItems?part=snippet&playlistId=%s&maxResults=50", url.QueryEscape(playlistID))
	reqCheck, err := http.NewRequest("GET", checkURL, nil)
	if err == nil {
		reqCheck.Header.Set("Authorization", "Bearer "+accessToken)
		if respCheck, errDo := client.Do(reqCheck); errDo == nil {
			if respCheck.StatusCode >= 400 {
				_ = apiErrFromResponse(respCheck, "playlistItems.list")
				respCheck.Body.Close()
			} else {
			var plData struct {
				Items []struct {
					Snippet struct {
						ResourceID struct {
							VideoID string `json:"videoId"`
						} `json:"resourceId"`
					} `json:"snippet"`
				} `json:"items"`
			}
			if json.NewDecoder(respCheck.Body).Decode(&plData) == nil {
				for _, it := range plData.Items {
					if it.Snippet.ResourceID.VideoID == videoID {
						respCheck.Body.Close()
						return nil // Sudah ada di playlist, lewati tanpa insert ganda
					}
				}
			}
			respCheck.Body.Close()
			}
		}
	}

	// 2. Belum ada -> tambahkan ke playlist
	reqBody := map[string]interface{}{
		"snippet": map[string]interface{}{
			"playlistId": playlistID,
			"resourceId": map[string]interface{}{
				"kind":    "youtube#video",
				"videoId": videoID,
			},
		},
	}
	payloadBytes, _ := json.Marshal(reqBody)

	apiURL := "https://www.googleapis.com/youtube/v3/playlistItems?part=snippet"
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("playlistItems.insert gagal (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func formatBroadcastTitle(template, cameraName string, t time.Time) string {
	if template == "" {
		template = "{name} - {date} {time} WIB"
	}
	res := template
	res = strings.ReplaceAll(res, "{name}", cameraName)
	res = strings.ReplaceAll(res, "{camera}", cameraName)
	res = strings.ReplaceAll(res, "{date}", t.Format("02/01/2006"))
	res = strings.ReplaceAll(res, "{day}", t.Format("02"))
	res = strings.ReplaceAll(res, "{month}", t.Format("01"))
	res = strings.ReplaceAll(res, "{year}", t.Format("2006"))
	res = strings.ReplaceAll(res, "{time}", t.Format("15:04"))
	res = strings.ReplaceAll(res, "{datetime}", t.Format("02/01/2006 15:04"))
	return res
}

// nextQuotaReset menghitung waktu reset kuota YouTube Data API berikutnya.
// Kuota Google direset tengah malam Pacific Time, yaitu ~14:30 WIB (UTC+7).
// Kalau sekarang sudah lewat 14:30, reset berikutnya besok.
func nextQuotaReset(now time.Time) time.Time {
	reset := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, now.Location())
	if !reset.After(now) {
		reset = reset.AddDate(0, 0, 1)
	}
	return reset
}

// apiErrFromResponse membaca body error Google API dan mengembalikan pesan yang jujur.
// Jika 403 quotaExceeded, set quotaExceededUntil supaya seluruh panggilan API berhenti.
func apiErrFromResponse(resp *http.Response, what string) error {
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if resp.StatusCode == 403 && strings.Contains(s, "quotaExceeded") {
		quotaMu.Lock()
		quotaExceededUntil = nextQuotaReset(time.Now())
		quotaMu.Unlock()
		return fmt.Errorf("kuota YouTube API habis (quotaExceeded). Panggilan API dihentikan sampai reset sekitar 14:30 WIB")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s gagal (HTTP %d): %s", what, resp.StatusCode, strings.TrimSpace(s))
	}
	return nil
}

func findYouTubeStreamID(accessToken, streamKey string) (string, error) {
	streamListURL := "https://www.googleapis.com/youtube/v3/liveStreams?part=id,cdn,status&mine=true&maxResults=10"
	reqStream, _ := http.NewRequest("GET", streamListURL, nil)
	reqStream.Header.Set("Authorization", "Bearer "+accessToken)
	client := &http.Client{Timeout: 10 * time.Second}
	respStream, err := client.Do(reqStream)
	if err != nil {
		return "", fmt.Errorf("gagal ambil liveStreams: %v", err)
	}
	defer respStream.Body.Close()

	if respStream.StatusCode >= 400 {
		return "", apiErrFromResponse(respStream, "liveStreams.list")
	}

	var streamData struct {
		Items []struct {
			ID  string `json:"id"`
			CDN struct {
				IngestionInfo struct {
					StreamName string `json:"streamName"`
				} `json:"ingestionInfo"`
			} `json:"cdn"`
		} `json:"items"`
	}
	if err := json.NewDecoder(respStream.Body).Decode(&streamData); err != nil {
		return "", fmt.Errorf("decode liveStreams: %v", err)
	}

	for _, it := range streamData.Items {
		if strings.TrimSpace(it.CDN.IngestionInfo.StreamName) == strings.TrimSpace(streamKey) {
			return it.ID, nil
		}
	}
	if len(streamData.Items) > 0 {
		// Fallback: gunakan stream pertama di channel
		return streamData.Items[0].ID, nil
	}
	return "", fmt.Errorf("tidak ada live stream key terdaftar di channel YouTube")
}

// cleanStaleBroadcasts membersihkan broadcast berstatus "ready" / "created" yang tidak
// pernah menerima data live (> 1 jam yang lalu) agar tidak menumpuk video "menunggu".
func cleanStaleBroadcasts(accessToken, streamKey string, ring *RingBuffer) {
	client := &http.Client{Timeout: 10 * time.Second}
	targetStreamID, err := findYouTubeStreamID(accessToken, streamKey)
	if err != nil {
		return
	}

	listURL := "https://www.googleapis.com/youtube/v3/liveBroadcasts?part=id,snippet,status,contentDetails&mine=true&maxResults=50"
	reqList, _ := http.NewRequest("GET", listURL, nil)
	reqList.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(reqList)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		_ = apiErrFromResponse(resp, "liveBroadcasts.list")
		return
	}

	var data struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				ScheduledStartTime string `json:"scheduledStartTime"`
			} `json:"snippet"`
			Status struct {
				LifeCycleStatus string `json:"lifeCycleStatus"`
			} `json:"status"`
			ContentDetails struct {
				BoundStreamID string `json:"boundStreamId"`
			} `json:"contentDetails"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return
	}

	now := time.Now()
	for _, it := range data.Items {
		if it.ContentDetails.BoundStreamID != targetStreamID {
			continue
		}
		// Hapus broadcast yang masih ready/created lebih dari 30 menit lalu
		if it.Status.LifeCycleStatus == "ready" || it.Status.LifeCycleStatus == "created" {
			t, err := time.Parse(time.RFC3339, it.Snippet.ScheduledStartTime)
			if err == nil && now.Sub(t) > 30*time.Minute {
				delURL := fmt.Sprintf("https://www.googleapis.com/youtube/v3/liveBroadcasts?id=%s", url.QueryEscape(it.ID))
				delReq, _ := http.NewRequest("DELETE", delURL, nil)
				delReq.Header.Set("Authorization", "Bearer "+accessToken)
				if dResp, dErr := client.Do(delReq); dErr == nil {
					dResp.Body.Close()
					if ring != nil {
						ring.Append(fmt.Sprintf("[YouTube] Membersihkan broadcast kedaluwarsa: %s", it.ID))
					}
				}
			}
		}
	}
}

// ensureYouTubeBroadcast mencari broadcast yang masih hidup (ready/testing/live) terikat
// pada stream key kamera dan menggunakannya kembali. Hanya membuat broadcast baru jika
// tidak ada yang cocok — mencegah duplikat siaran di playlist.
func ensureYouTubeBroadcast(accessToken, streamKey, cameraName, titleTemplate string, allowCreate bool) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	targetStreamID, err := findYouTubeStreamID(accessToken, streamKey)
	if err != nil {
		return "", err
	}

	// 1. Cari broadcast lama terikat stream key yang AKTIF ('live' atau 'liveStarting') → reuse.
	// Catatan: JANGAN reuse broadcast yang masih 'ready' jika dibuat lebih dari 2 menit lalu,
	// karena siaran 'ready' yang sempat kehilangan trigger enableAutoStart tidak akan pernah live.
	listURL := "https://www.googleapis.com/youtube/v3/liveBroadcasts?part=id,snippet,status,contentDetails&mine=true&maxResults=50"
	reqList, _ := http.NewRequest("GET", listURL, nil)
	reqList.Header.Set("Authorization", "Bearer "+accessToken)
	if resp, err := client.Do(reqList); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return "", apiErrFromResponse(resp, "liveBroadcasts.list")
		}
		var data struct {
			Items []struct {
				ID      string `json:"id"`
				Snippet struct {
					ScheduledStartTime string `json:"scheduledStartTime"`
				} `json:"snippet"`
				Status struct {
					LifeCycleStatus string `json:"lifeCycleStatus"`
				} `json:"status"`
				ContentDetails struct {
					BoundStreamID string `json:"boundStreamId"`
				} `json:"contentDetails"`
			} `json:"items"`
		}
		if json.NewDecoder(resp.Body).Decode(&data) == nil {
			for _, it := range data.Items {
				if it.ContentDetails.BoundStreamID == targetStreamID {
					if it.Status.LifeCycleStatus == "live" || it.Status.LifeCycleStatus == "liveStarting" {
						return it.ID, nil
					}
					// Jika ready tapi baru dibuat (< 90 detik), masih boleh reuse
					if it.Status.LifeCycleStatus == "ready" {
						if tStart, errT := time.Parse(time.RFC3339, it.Snippet.ScheduledStartTime); errT == nil {
							if time.Since(tStart) < 90*time.Second {
								return it.ID, nil
							}
						}
					}
				}
			}
		}
	}

	// 2. Belum ada → buat broadcast baru (Unlisted, autoStart, autoStop)
	// Guard: jika allowCreate=false (broadcast baru saja dibuat <15 menit lalu),
	// JANGAN buat baru — hemat kuota API. Biarkan siaran lama dipakai / user tunggu.
	if !allowCreate {
		return "", fmt.Errorf("broadcast guard aktif: siaran baru saja dibuat <15 menit lalu, tidak membuat broadcast baru demi menghemat kuota API")
	}

	now := time.Now()
	title := formatBroadcastTitle(titleTemplate, cameraName, now)
	createBody := map[string]interface{}{
		"snippet": map[string]interface{}{
			"title":              title,
			"scheduledStartTime": now.UTC().Format(time.RFC3339),
		},
		"status": map[string]interface{}{
			"privacyStatus":           "unlisted",
			"selfDeclaredMadeForKids": false,
		},
		"contentDetails": map[string]interface{}{
			"enableAutoStart": true,
			"enableAutoStop":  true,
			"recordFromStart": true,
		},
	}
	bBytes, _ := json.Marshal(createBody)
	createReq, _ := http.NewRequest("POST", "https://www.googleapis.com/youtube/v3/liveBroadcasts?part=snippet,status,contentDetails", bytes.NewReader(bBytes))
	createReq.Header.Set("Authorization", "Bearer "+accessToken)
	createReq.Header.Set("Content-Type", "application/json")

	createResp, err := client.Do(createReq)
	if err != nil {
		return "", fmt.Errorf("gagal create liveBroadcast: %v", err)
	}
	defer createResp.Body.Close()

	if createResp.StatusCode >= 400 {
		errBytes, _ := io.ReadAll(createResp.Body)
		return "", fmt.Errorf("createBroadcast status %d: %s", createResp.StatusCode, string(errBytes))
	}

	var createdBroadcast struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&createdBroadcast); err != nil || createdBroadcast.ID == "" {
		return "", fmt.Errorf("gagal decode created broadcast ID")
	}

	// 3. Bind Broadcast ke Stream Key
	bindURL := fmt.Sprintf("https://www.googleapis.com/youtube/v3/liveBroadcasts/bind?id=%s&part=id,contentDetails&streamId=%s",
		url.QueryEscape(createdBroadcast.ID), url.QueryEscape(targetStreamID))
	bindReq, _ := http.NewRequest("POST", bindURL, nil)
	bindReq.Header.Set("Authorization", "Bearer "+accessToken)
	bindResp, err := client.Do(bindReq)
	if err != nil {
		return "", fmt.Errorf("gagal bind broadcast: %v", err)
	}
	defer bindResp.Body.Close()

	return createdBroadcast.ID, nil
}

// waitForBroadcastLive memantau lifecycle broadcast sampai benar-benar "live"
// (maksimal 2 menit) agar log mencerminkan status siaran sesungguhnya.
func waitForBroadcastLive(accessToken, videoID string, ring *RingBuffer) string {
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(2 * time.Minute)
	last := ""
	for time.Now().Before(deadline) {
		apiURL := fmt.Sprintf("https://www.googleapis.com/youtube/v3/liveBroadcasts?part=status&id=%s", url.QueryEscape(videoID))
		req, _ := http.NewRequest("GET", apiURL, nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		resp, err := client.Do(req)
		if err == nil {
			var data struct {
				Items []struct {
					Status struct {
						LifeCycleStatus string `json:"lifeCycleStatus"`
					} `json:"status"`
				} `json:"items"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()
			if len(data.Items) > 0 {
				lc := data.Items[0].Status.LifeCycleStatus
				if lc != last {
					ring.Append(fmt.Sprintf("[YouTube] Broadcast %s status: %s", videoID, lc))
					last = lc
				}
				if lc == "live" {
					return lc
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	return last
}

// getYouTubeStreamHealth mengambil status ingest stream key: active/ready + good/noData/error.
func getYouTubeStreamHealth(accessToken, streamKey string) (string, string, error) {
	apiURL := "https://www.googleapis.com/youtube/v3/liveStreams?part=id,cdn,status&mine=true&maxResults=10"
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", "", apiErrFromResponse(resp, "liveStreams.list")
	}

	var data struct {
		Items []struct {
			CDN struct {
				IngestionInfo struct {
					StreamName string `json:"streamName"`
				} `json:"ingestionInfo"`
			} `json:"cdn"`
			Status struct {
				StreamStatus string `json:"streamStatus"`
				HealthStatus struct {
					Status string `json:"status"`
				} `json:"healthStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", "", err
	}
	for _, it := range data.Items {
		if strings.TrimSpace(it.CDN.IngestionInfo.StreamName) == strings.TrimSpace(streamKey) {
			return it.Status.StreamStatus, it.Status.HealthStatus.Status, nil
		}
	}
	return "", "", fmt.Errorf("stream key tidak ditemukan")
}

// --- Quota exceeded guard ---
var (
	quotaMu            sync.RWMutex
	quotaExceededUntil time.Time

	lastCreateMu        sync.Mutex
	lastBroadcastCreate = make(map[string]time.Time)
)

type Camera struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	RTSPUrl              string `json:"rtsp_url"`
	RTSPSubUrl           string `json:"rtsp_sub_url,omitempty"`
	RTMPUrl              string `json:"rtmp_url"`
	StreamKey            string `json:"stream_key"`
	LiveUrl              string `json:"live_url"`
	YouTubePlaylistID    string `json:"youtube_playlist_id,omitempty"`
	YouTubeTitleTemplate string `json:"youtube_title_template,omitempty"`
	AutoStart            bool   `json:"auto_start"`
	EnableYouTube        bool   `json:"enable_youtube"`
	RecordHDD            bool   `json:"record_hdd"`
	RetentionDays        int    `json:"retention_days"`
	// Legacy backward-compat fields (derived from sub-statuses)
	Status    string `json:"status"` // LIVE, STOPPED, ERROR
	StartedAt int64  `json:"started_at"`
	ErrorMsg  string `json:"error_msg"`
	Uptime    string `json:"uptime,omitempty"`
	// New independent worker statuses
	RecordStatus    string `json:"record_status"` // RECORDING, STOPPED, ERROR
	StreamStatus    string `json:"stream_status"` // STREAMING, STOPPED, ERROR, OFFLINE
	RecordStartedAt int64  `json:"record_started_at"`
	StreamStartedAt int64  `json:"stream_started_at"`
	RecordUptime    string `json:"record_uptime,omitempty"`
	StreamUptime    string `json:"stream_uptime,omitempty"`
}

type RingBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func NewRingBuffer(max int) *RingBuffer {
	if max <= 0 {
		max = 100
	}
	return &RingBuffer{max: max, lines: make([]string, 0, max)}
}

func (r *RingBuffer) Append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Jangan simpan spam log frame FFmpeg yang berganti tiap 0.5 detik
	if strings.HasPrefix(line, "size=") || strings.Contains(line, "Non-monotonic DTS") {
		return
	}
	if len(r.lines) >= r.max {
		r.lines = r.lines[1:]
	}
	r.lines = append(r.lines, line)
}

func (r *RingBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

type CameraStore struct {
	mu       sync.RWMutex
	filePath string
	cameras  []Camera
}

func NewCameraStore(filePath string) (*CameraStore, error) {
	cs := &CameraStore{filePath: filePath, cameras: make([]Camera, 0)}
	if err := cs.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return cs, nil
}

func (cs *CameraStore) load() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	data, err := os.ReadFile(cs.filePath)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		cs.cameras = make([]Camera, 0)
		return nil
	}
	if err := json.Unmarshal(data, &cs.cameras); err != nil {
		return err
	}
	for i := range cs.cameras {
		if cs.cameras[i].RetentionDays <= 0 {
			cs.cameras[i].RetentionDays = 7
		}
		if !cs.cameras[i].EnableYouTube && !cs.cameras[i].RecordHDD {
			cs.cameras[i].EnableYouTube = true
			cs.cameras[i].RecordHDD = true
		}
	}
	return nil
}

func (cs *CameraStore) save() error {
	dir := filepath.Dir(cs.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cs.cameras, "", "  ")
	if err != nil {
		return err
	}
	tmpFile := cs.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, cs.filePath)
}

func (cs *CameraStore) GetAll() []Camera {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	result := make([]Camera, len(cs.cameras))
	copy(result, cs.cameras)
	return result
}

func (cs *CameraStore) Get(id string) (Camera, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	for _, c := range cs.cameras {
		if c.ID == id {
			return c, true
		}
	}
	return Camera{}, false
}

func (cs *CameraStore) Add(c Camera) (Camera, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if c.ID == "" {
		c.ID = newID()
	}
	if c.Status == "" {
		c.Status = "STOPPED"
	}
	if c.RecordStatus == "" {
		c.RecordStatus = "STOPPED"
	}
	if c.StreamStatus == "" {
		c.StreamStatus = "STOPPED"
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = 7
	}
	cs.cameras = append(cs.cameras, c)
	return c, cs.save()
}

func (cs *CameraStore) Update(c Camera) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	found := false
	for i, existing := range cs.cameras {
		if existing.ID == c.ID {
			if c.RetentionDays <= 0 {
				c.RetentionDays = 7
			}
			cs.cameras[i] = c
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("camera %s not found", c.ID)
	}
	return cs.save()
}

// SetStatus backward-compat: updates both sub-statuses
func (cs *CameraStore) SetStatus(id, status, errorMsg string, startedAt int64) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i, c := range cs.cameras {
		if c.ID == id {
			cs.cameras[i].Status = status
			cs.cameras[i].ErrorMsg = errorMsg
			cs.cameras[i].StartedAt = startedAt
			break
		}
	}
	_ = cs.save()
}

func (cs *CameraStore) SetRecordStatus(id, status, errorMsg string, startedAt int64) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i, c := range cs.cameras {
		if c.ID == id {
			cs.cameras[i].RecordStatus = status
			if errorMsg != "" {
				cs.cameras[i].ErrorMsg = errorMsg
			} else if status == "RECORDING" && cs.cameras[i].StreamStatus != "ERROR" && cs.cameras[i].StreamStatus != "OFFLINE" {
				cs.cameras[i].ErrorMsg = ""
			}
			cs.cameras[i].RecordStartedAt = startedAt
			// Derive legacy Status
			cs.cameras[i].Status = deriveLegacyStatus(status, cs.cameras[i].StreamStatus)
			if cs.cameras[i].Status == "LIVE" && cs.cameras[i].StartedAt == 0 {
				cs.cameras[i].StartedAt = startedAt
			}
			if cs.cameras[i].Status == "STOPPED" {
				cs.cameras[i].StartedAt = 0
				cs.cameras[i].ErrorMsg = ""
			}
			break
		}
	}
	_ = cs.save()
}

func (cs *CameraStore) SetStreamStatus(id, status, errorMsg string, startedAt int64) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i, c := range cs.cameras {
		if c.ID == id {
			cs.cameras[i].StreamStatus = status
			if errorMsg != "" {
				cs.cameras[i].ErrorMsg = errorMsg
			} else if status == "STREAMING" && cs.cameras[i].RecordStatus != "ERROR" {
				cs.cameras[i].ErrorMsg = ""
			}
			cs.cameras[i].StreamStartedAt = startedAt
			// Derive legacy Status
			cs.cameras[i].Status = deriveLegacyStatus(cs.cameras[i].RecordStatus, status)
			if cs.cameras[i].Status == "LIVE" && cs.cameras[i].StartedAt == 0 {
				cs.cameras[i].StartedAt = startedAt
			}
			if cs.cameras[i].Status == "STOPPED" {
				cs.cameras[i].StartedAt = 0
				cs.cameras[i].ErrorMsg = ""
			}
			break
		}
	}
	_ = cs.save()
}

func deriveLegacyStatus(recordStatus, streamStatus string) string {
	if recordStatus == "RECORDING" || streamStatus == "STREAMING" {
		return "LIVE"
	}
	if recordStatus == "ERROR" || streamStatus == "ERROR" || streamStatus == "OFFLINE" {
		return "ERROR"
	}
	return "STOPPED"
}

func (cs *CameraStore) Delete(id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	newSlice := make([]Camera, 0, len(cs.cameras))
	found := false
	for _, c := range cs.cameras {
		if c.ID == id {
			found = true
			continue
		}
		newSlice = append(newSlice, c)
	}
	if !found {
		return fmt.Errorf("camera %s not found", id)
	}
	cs.cameras = newSlice
	return cs.save()
}

// WorkerSession tracks a single FFmpeg worker (either record or stream)
type WorkerSession struct {
	cancel context.CancelFunc
	ring   *RingBuffer
}

// CameraWorkers holds independent record and stream workers
type CameraWorkers struct {
	record *WorkerSession
	stream *WorkerSession
}

// StreamLogs memisahkan log teknis kamera (RTSP/HDD/FFmpeg)
// dari log otomatisasi YouTube (RTMP/API/broadcast) agar mudah diagnosa.
type StreamLogs struct {
	Camera  *RingBuffer // RTSP, FFmpeg, segmenter HDD
	YouTube *RingBuffer // API OAuth, broadcast lifecycle, playlist, watchdog
}

type StreamEngine struct {
	mu            sync.Mutex
	sessions      map[string]*CameraWorkers
	logs          map[string]*StreamLogs
	store         *CameraStore
	settingsStore *SettingsStore
}

func NewStreamEngine(store *CameraStore, settingsStore *SettingsStore) *StreamEngine {
	return &StreamEngine{
		sessions:      make(map[string]*CameraWorkers),
		logs:          make(map[string]*StreamLogs),
		store:         store,
		settingsStore: settingsStore,
	}
}

func (e *StreamEngine) getLogs(id string) *StreamLogs {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.getLogsLocked(id)
}

func (e *StreamEngine) getLogsLocked(id string) *StreamLogs {
	l, ok := e.logs[id]
	if !ok || l == nil {
		l = &StreamLogs{Camera: NewRingBuffer(200), YouTube: NewRingBuffer(150)}
		e.logs[id] = l
	}
	return l
}

func (e *StreamEngine) getWorkersLocked(id string) *CameraWorkers {
	w, ok := e.sessions[id]
	if !ok || w == nil {
		w = &CameraWorkers{}
		e.sessions[id] = w
	}
	return w
}

// rtspReachable does TCP dial to camera RTSP port with 2s timeout
func rtspReachable(rtspURL string) bool {
	u, err := url.Parse(rtspURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "554"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Start starts both record and stream workers (backward compat for AutoStart)
func (e *StreamEngine) Start(id string) error {
	cam, ok := e.store.Get(id)
	if !ok {
		return fmt.Errorf("camera not found")
	}

	var errR, errS error
	if cam.RecordHDD {
		errR = e.StartRecord(id)
	}
	if cam.EnableYouTube && cam.StreamKey != "" {
		errS = e.StartStream(id)
	}
	if !cam.RecordHDD && !(cam.EnableYouTube && cam.StreamKey != "") {
		return fmt.Errorf("neither YouTube nor HDD recording is enabled for camera %s", cam.Name)
	}
	if errR != nil {
		return errR
	}
	return errS
}

// Stop stops both workers (backward compat)
func (e *StreamEngine) Stop(id string) error {
	e.StopRecord(id)
	e.StopStream(id)
	return nil
}

// StartRecord starts only HDD recording FFmpeg worker
func (e *StreamEngine) StartRecord(id string) error {
	e.mu.Lock()

	cam, ok := e.store.Get(id)
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("camera not found")
	}
	if !cam.RecordHDD {
		e.mu.Unlock()
		return fmt.Errorf("HDD recording tidak diaktifkan untuk kamera %s", cam.Name)
	}

	workers := e.getWorkersLocked(id)
	if workers.record != nil && workers.record.cancel != nil {
		e.mu.Unlock()
		return nil // already running
	}

	ctx, cancel := context.WithCancel(context.Background())
	logs := e.getLogsLocked(id)
	workers.record = &WorkerSession{cancel: cancel, ring: logs.Camera}

	now := time.Now().Unix()
	e.store.SetRecordStatus(id, "RECORDING", "", now)
	e.mu.Unlock()

	go e.runRecordWorker(ctx, id, logs.Camera)
	return nil
}

// StopRecord stops only HDD recording FFmpeg worker
func (e *StreamEngine) StopRecord(id string) error {
	e.mu.Lock()
	workers, exists := e.sessions[id]
	if exists && workers != nil && workers.record != nil && workers.record.cancel != nil {
		workers.record.cancel()
		workers.record = nil
	}
	e.mu.Unlock()

	e.store.SetRecordStatus(id, "STOPPED", "", 0)
	return nil
}

// StartStream starts only YouTube RTMP FFmpeg worker (with pre-flight RTSP check + broadcast creation)
func (e *StreamEngine) StartStream(id string) error {
	cam, ok := e.store.Get(id)
	if !ok {
		return fmt.Errorf("camera not found")
	}
	if !cam.EnableYouTube || cam.StreamKey == "" {
		return fmt.Errorf("YouTube streaming tidak diaktifkan atau stream key kosong untuk kamera %s", cam.Name)
	}

	// Quota exceeded guard (checked before anything expensive)
	quotaMu.RLock()
	blocked := quotaExceededUntil.After(time.Now())
	quotaMu.RUnlock()
	if blocked {
		quotaReset := nextQuotaReset(time.Now())
		return fmt.Errorf("Kuota YouTube API habis. Reset berikutnya sekitar %s WIB", quotaReset.Format("15:04"))
	}

	// Pre-flight RTSP check: TCP dial before any YouTube API call.
	// Dilakukan DI LUAR lock supaya tidak memblokir engine 2 detik.
	if !rtspReachable(cam.RTSPUrl) {
		e.store.SetStreamStatus(id, "OFFLINE", "Kamera offline", 0)
		return fmt.Errorf("Kamera offline, tidak bisa memulai live YouTube")
	}

	e.mu.Lock()
	workers := e.getWorkersLocked(id)
	if workers.stream != nil && workers.stream.cancel != nil {
		e.mu.Unlock()
		return nil // already running
	}

	ctx, cancel := context.WithCancel(context.Background())
	logs := e.getLogsLocked(id)
	workers.stream = &WorkerSession{cancel: cancel, ring: logs.YouTube}

	now := time.Now().Unix()
	e.store.SetStreamStatus(id, "STREAMING", "", now)
	e.mu.Unlock()

	go func() {
		// Ensure YouTube broadcast before starting FFmpeg
		s := e.settingsStore.Get()
		playlistID := cam.YouTubePlaylistID
		if playlistID == "" {
			playlistID = s.YouTubePlaylistID
		}

		if s.YouTubeClientID != "" && s.YouTubeClientSecret != "" && s.YouTubeRefreshToken != "" {
			var token string
			var err error
			for attempt := 1; attempt <= 3; attempt++ {
				token, err = getYouTubeAccessToken(s.YouTubeClientID, s.YouTubeClientSecret, s.YouTubeRefreshToken)
				if err == nil {
					break
				}
				if attempt < 3 {
					time.Sleep(4 * time.Second)
				}
			}
			if err != nil {
				logs.YouTube.Append(fmt.Sprintf("[YouTube Auto-Playlist] Gagal refresh token: %v", err))
			} else {
				// Active Broadcast Guard: reject new creation if <15 minutes since last create
				lastCreateMu.Lock()
				allowCreate := true
				if last, ok := lastBroadcastCreate[cam.StreamKey]; ok && time.Since(last) < 15*time.Minute {
					allowCreate = false
				}
				lastCreateMu.Unlock()

				cleanStaleBroadcasts(token, cam.StreamKey, logs.YouTube)
				videoID, errBc := ensureYouTubeBroadcast(token, cam.StreamKey, cam.Name, cam.YouTubeTitleTemplate, allowCreate)
				if errBc != nil {
					errStr := errBc.Error()
					if strings.Contains(errStr, "quotaExceeded") || strings.Contains(errStr, "kuota YouTube API habis") {
						logs.YouTube.Append("[YouTube] KUOTA API HABIS. Stream YouTube dihentikan agar tidak memompa RTMP tanpa siaran. Rekaman HDD tetap jalan.")
						e.StopStream(id)
						e.store.SetStreamStatus(id, "OFFLINE", "Kuota YouTube API habis, tunggu reset sekitar "+nextQuotaReset(time.Now()).Format("15:04")+" WIB", 0)
						return
					}
					logs.YouTube.Append(fmt.Sprintf("[YouTube Auto-Broadcast] %v", errBc))
				} else {
					// Track last broadcast creation time
					lastCreateMu.Lock()
					lastBroadcastCreate[cam.StreamKey] = time.Now()
					lastCreateMu.Unlock()

					if playlistID != "" {
						if errPl := addVideoToYouTubePlaylist(token, playlistID, videoID); errPl != nil {
							logs.YouTube.Append(fmt.Sprintf("[YouTube Auto-Playlist] Gagal sync playlist: %v", errPl))
						} else {
							logs.YouTube.Append(fmt.Sprintf("[YouTube Auto-Playlist] SUKSES! Siaran otomatis dibuat & sync ke playlist (%s): https://youtu.be/%s", playlistID, videoID))
						}
					}
					go waitForBroadcastLive(token, videoID, logs.YouTube)
				}
			}
		}

		// Run FFmpeg stream worker
		e.runStreamWorker(ctx, id, logs.YouTube)
	}()

	return nil
}

// StopStream stops only YouTube RTMP FFmpeg worker
func (e *StreamEngine) StopStream(id string) error {
	e.mu.Lock()
	workers, exists := e.sessions[id]
	if exists && workers != nil && workers.stream != nil && workers.stream.cancel != nil {
		workers.stream.cancel()
		workers.stream = nil
	}
	e.mu.Unlock()

	e.store.SetStreamStatus(id, "STOPPED", "", 0)
	return nil
}

func (e *StreamEngine) StopAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, workers := range e.sessions {
		if workers != nil {
			if workers.record != nil && workers.record.cancel != nil {
				workers.record.cancel()
			}
			if workers.stream != nil && workers.stream.cancel != nil {
				workers.stream.cancel()
			}
		}
		delete(e.sessions, id)
		e.store.SetRecordStatus(id, "STOPPED", "", 0)
		e.store.SetStreamStatus(id, "STOPPED", "", 0)
	}
}

// GetLogs mengembalikan log terpisah: channel "camera" (RTSP/FFmpeg/HDD)
// dan channel "youtube" (API/broadcast/playlist).
func (e *StreamEngine) GetLogs(id, channel string) string {
	e.mu.Lock()
	l, exists := e.logs[id]
	e.mu.Unlock()
	if !exists || l == nil {
		return "Belum ada log aktivitas untuk kamera ini."
	}
	if channel == "youtube" {
		return l.YouTube.String()
	}
	return l.Camera.String()
}

func (e *StreamEngine) ActiveCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, w := range e.sessions {
		if w != nil {
			if w.record != nil && w.record.cancel != nil {
				count++
			}
			if w.stream != nil && w.stream.cancel != nil {
				count++
			}
		}
	}
	return count
}

func getNextSegmentStartNum(camID, dateFolder string) int {
	dir := filepath.Join("/mnt/hdd/cctv", camID, dateFolder)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}

	prefix := dateFolder + "-"
	maxX := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".mkv") {
			numStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".mkv")
			if num, err := strconv.Atoi(numStr); err == nil && num > maxX {
				maxX = num
			}
		}
	}
	return maxX + 1
}

// pipeStderr reads FFmpeg stderr and appends to ring buffer
func pipeStderr(r io.Reader, ring *RingBuffer) {
	buf := make([]byte, 4096)
	var curLine strings.Builder
	for {
		n, err := r.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				if b == '\n' || b == '\r' {
					line := strings.TrimSpace(curLine.String())
					if line != "" {
						ring.Append(line)
					}
					curLine.Reset()
				} else {
					curLine.WriteByte(b)
				}
			}
		}
		if err != nil {
			line := strings.TrimSpace(curLine.String())
			if line != "" {
				ring.Append(line)
			}
			break
		}
	}
}

// runRecordWorker runs FFmpeg loop that ONLY does HDD MKV segment recording.
// On RTSP failure, retry with backoff. No YouTube API calls ever.
func (e *StreamEngine) runRecordWorker(ctx context.Context, id string, ring *RingBuffer) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cam, ok := e.store.Get(id)
		if !ok {
			return
		}

		currSettings := e.settingsStore.Get()

		todayFolder := time.Now().Format("02-01-2006")
		dir := filepath.Join("/mnt/hdd/cctv", cam.ID, todayFolder)
		_ = os.MkdirAll(dir, 0755)

		startNum := getNextSegmentStartNum(cam.ID, todayFolder)
		outPattern := fmt.Sprintf("/mnt/hdd/cctv/%s/%s/%s-%%d.mkv", cam.ID, todayFolder, todayFolder)

		segSec := currSettings.RecordSegmentSec
		if segSec <= 0 {
			segSec = 3600
		}

		args := []string{
			"-nostdin",
			"-use_wallclock_as_timestamps", "1",
			"-fflags", "+genpts+discardcorrupt",
			"-rtsp_transport", "tcp",
			"-timeout", "5000000",
			"-i", cam.RTSPUrl,
			"-map", "0:v", "-map", "0:a",
			"-c:v", "copy",
			"-c:a", "aac", "-b:a", "64k", "-ar", "44100", "-ac", "2",
			"-af", "aresample=async=1000",
			"-max_muxing_queue_size", "2048",
			"-avoid_negative_ts", "make_zero",
			"-f", "segment",
			"-segment_time", strconv.Itoa(segSec),
			"-segment_start_number", strconv.Itoa(startNum),
			"-segment_format", "matroska",
			"-reset_timestamps", "1",
			outPattern,
		}

		ring.Append(fmt.Sprintf("[%s] [Record] Starting FFmpeg HDD recording for %s...", time.Now().Format("15:04:05"), cam.Name))

		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		stderr, err := cmd.StderrPipe()
		if err != nil {
			msg := fmt.Sprintf("[%s] [Record] Stderr pipe error: %v", time.Now().Format("15:04:05"), err)
			ring.Append(msg)
			e.store.SetRecordStatus(id, "ERROR", err.Error(), 0)
			time.Sleep(3 * time.Second)
			continue
		}

		if err := cmd.Start(); err != nil {
			msg := fmt.Sprintf("[%s] [Record] Failed to launch FFmpeg: %v", time.Now().Format("15:04:05"), err)
			ring.Append(msg)
			e.store.SetRecordStatus(id, "ERROR", err.Error(), 0)
			time.Sleep(5 * time.Second)
			continue
		}

		go pipeStderr(stderr, ring)

		waitErr := cmd.Wait()

		select {
		case <-ctx.Done():
			ring.Append(fmt.Sprintf("[%s] [Record] FFmpeg recording terminated by user request.", time.Now().Format("15:04:05")))
			return
		default:
		}

		if waitErr != nil {
			errMsg := fmt.Sprintf("[%s] [Record] FFmpeg terhenti (koneksi kamera RTSP putus): %v. Mencoba sambung kembali dalam 5s...", time.Now().Format("15:04:05"), waitErr)
			ring.Append(errMsg)
			e.store.SetRecordStatus(id, "RECORDING", "Koneksi terputus, mencoba sambung ulang...", cam.RecordStartedAt)
		} else {
			ring.Append(fmt.Sprintf("[%s] [Record] FFmpeg keluar normal. Menyambung ulang dalam 3s...", time.Now().Format("15:04:05")))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// runStreamWorker runs FFmpeg loop that ONLY does YouTube RTMP streaming.
// Has rotation timer (715 min). On RTSP failure for >3 minutes continuously,
// AUTO-STOP YouTube streaming and set StreamStatus='OFFLINE'.
func (e *StreamEngine) runStreamWorker(ctx context.Context, id string, ring *RingBuffer) {
	var rtspFailStart time.Time
	rtspFailing := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cam, ok := e.store.Get(id)
		if !ok {
			return
		}

		if !cam.EnableYouTube || cam.StreamKey == "" {
			ring.Append(fmt.Sprintf("[%s] [Stream] YouTube streaming tidak aktif. Worker berhenti.", time.Now().Format("15:04:05")))
			e.store.SetStreamStatus(id, "STOPPED", "", 0)
			return
		}

		target := strings.TrimRight(cam.RTMPUrl, "/") + "/" + strings.TrimLeft(cam.StreamKey, "/")

		args := []string{
			"-nostdin",
			"-use_wallclock_as_timestamps", "1",
			"-fflags", "+genpts+discardcorrupt",
			"-rtsp_transport", "tcp",
			"-timeout", "5000000",
			"-i", cam.RTSPUrl,
			"-map", "0:v", "-map", "0:a",
			"-c:v", "copy",
			"-c:a", "aac", "-b:a", "128k", "-ar", "44100", "-ac", "2",
			"-af", "aresample=async=1000:min_hard_comp=0.100000:first_pts=0",
			"-max_muxing_queue_size", "2048",
			"-avoid_negative_ts", "make_zero",
			"-f", "flv",
			"-flvflags", "no_duration_filesize",
			"-rw_timeout", "5000000",
			target,
		}

		ring.Append(fmt.Sprintf("[%s] [Stream] Starting FFmpeg YouTube RTMP for %s...", time.Now().Format("15:04:05"), cam.Name))

		currSettings := e.settingsStore.Get()
		rotMins := currSettings.LiveDurationMinutes
		if rotMins <= 0 {
			rotMins = 715
		}

		sessionCtx, sessionCancel := context.WithTimeout(ctx, time.Duration(rotMins)*time.Minute)

		cmd := exec.CommandContext(sessionCtx, "ffmpeg", args...)
		stderr, err := cmd.StderrPipe()
		if err != nil {
			sessionCancel()
			msg := fmt.Sprintf("[%s] [Stream] Stderr pipe error: %v", time.Now().Format("15:04:05"), err)
			ring.Append(msg)
			e.store.SetStreamStatus(id, "ERROR", err.Error(), 0)
			time.Sleep(3 * time.Second)
			continue
		}

		if err := cmd.Start(); err != nil {
			sessionCancel()
			msg := fmt.Sprintf("[%s] [Stream] Failed to launch FFmpeg: %v", time.Now().Format("15:04:05"), err)
			ring.Append(msg)
			e.store.SetStreamStatus(id, "ERROR", err.Error(), 0)
			time.Sleep(5 * time.Second)
			continue
		}

		// RTSP connected successfully — reset failure tracking
		rtspFailing = false
		rtspFailStart = time.Time{}

		go pipeStderr(stderr, ring)

		waitErr := cmd.Wait()
		sessionCancel()

		select {
		case <-ctx.Done():
			ring.Append(fmt.Sprintf("[%s] [Stream] FFmpeg streaming terminated by user request.", time.Now().Format("15:04:05")))
			return
		default:
		}

		if sessionCtx.Err() == context.DeadlineExceeded {
			ring.Append(fmt.Sprintf("[%s] [Stream] Sesi %dm tercapai. Auto-rotate ke sesi live YouTube berikutnya dalam 5s...", time.Now().Format("15:04:05"), rotMins))
			// Reset failure tracking on rotation
			rtspFailing = false
			rtspFailStart = time.Time{}
		} else if waitErr != nil {
			ring.Append(fmt.Sprintf("[%s] [Stream] FFmpeg terhenti (koneksi RTSP/RTMP putus): %v. Mencoba sambung kembali dalam 5s...", time.Now().Format("15:04:05"), waitErr))

			// Track RTSP failure duration
			if !rtspFailing {
				rtspFailing = true
				rtspFailStart = time.Now()
			}

			// If RTSP has been failing for >3 minutes, auto-stop and go OFFLINE
			if rtspFailing && time.Since(rtspFailStart) > 3*time.Minute {
				ring.Append(fmt.Sprintf("[%s] [Stream] Kamera RTSP tidak tersambung >3 menit. YouTube Live dihentikan otomatis. Klik Mulai Live untuk memulai ulang.", time.Now().Format("15:04:05")))
				// Release worker slot + cancel context (hindari kebocoran goroutine/context)
				e.mu.Lock()
				if w, ok := e.sessions[id]; ok && w != nil {
					if w.stream != nil && w.stream.cancel != nil {
						w.stream.cancel()
					}
					w.stream = nil
				}
				e.mu.Unlock()
				e.store.SetStreamStatus(id, "OFFLINE", "Kamera offline >3 menit", 0)
				return
			}

			e.store.SetStreamStatus(id, "STREAMING", "Koneksi terputus, mencoba sambung ulang...", cam.StreamStartedAt)
		} else {
			ring.Append(fmt.Sprintf("[%s] [Stream] FFmpeg keluar normal. Menyambung ulang dalam 3s...", time.Now().Format("15:04:05")))
			rtspFailing = false
			rtspFailStart = time.Time{}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

func formatUptime(startedAt int64) string {
	if startedAt <= 0 {
		return "-"
	}
	diff := time.Now().Unix() - startedAt
	if diff < 0 {
		return "0s"
	}
	h := diff / 3600
	m := (diff % 3600) / 60
	s := diff % 60
	if h > 0 {
		return fmt.Sprintf("%02dh %02dm %02ds", h, m, s)
	}
	return fmt.Sprintf("%02dm %02ds", m, s)
}

type cpuTracker struct {
	mu        sync.Mutex
	lastTotal uint64
	lastIdle  uint64
	lastCheck time.Time
	cachedVal float64
}

var cpuState cpuTracker

func getDiskStats(mountPoint string) (totalGB, usedGB, freeGB, percent float64, err error) {
	out, execErr := exec.Command("df", "-P", "-k", mountPoint).Output()
	if execErr != nil {
		return 0, 0, 0, 0, execErr
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, 0, 0, 0, fmt.Errorf("unexpected df output")
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return 0, 0, 0, 0, fmt.Errorf("malformed df fields")
	}
	totalK, _ := strconv.ParseFloat(fields[1], 64)
	usedK, _ := strconv.ParseFloat(fields[2], 64)
	freeK, _ := strconv.ParseFloat(fields[3], 64)

	totalGB = float64(int((totalK/(1024*1024))*10)) / 10.0
	usedGB = float64(int((usedK/(1024*1024))*10)) / 10.0
	freeGB = float64(int((freeK/(1024*1024))*10)) / 10.0
	if totalK > 0 {
		percent = float64(int((usedK/totalK*100)*10)) / 10.0
	}
	return totalGB, usedGB, freeGB, percent, nil
}

func getSystemStats(activeStreams int) map[string]interface{} {
	stats := map[string]interface{}{
		"cpu":            getCpuUsage(),
		"ram":            0.0,
		"memory_used":    uint64(0),
		"memory_total":   uint64(0),
		"memory_free":    uint64(0),
		"active_streams": activeStreams,
		"disk_total_gb":  0.0,
		"disk_used_gb":   0.0,
		"disk_free_gb":   0.0,
		"disk_percent":   0.0,
		// Batas aman untuk live preview. STB punya ~1.8 GB; sisakan ruang untuk
		// perekam HDD + OS + page cache supaya tidak sampai OOM killer.
		"live_max":          4,
		"live_ram_mb":       46,   // terukur: RSS satu ffmpeg passthrough 720p
		"ram_guard_percent": 78.0, // di atas ini live dimatikan paksa
	}

	if totalGB, usedGB, freeGB, pct, err := getDiskStats("/mnt/hdd"); err == nil {
		stats["disk_total_gb"] = totalGB
		stats["disk_used_gb"] = usedGB
		stats["disk_free_gb"] = freeGB
		stats["disk_percent"] = pct
	}

	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		var totalKB, availKB uint64
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := scanner.Text()
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				key := strings.TrimSuffix(fields[0], ":")
				val, _ := strconv.ParseUint(fields[1], 10, 64)
				if key == "MemTotal" {
					totalKB = val
				} else if key == "MemAvailable" {
					availKB = val
				}
			}
		}
		if totalKB > 0 {
			usedKB := totalKB - availKB
			ramPercent := float64(usedKB) / float64(totalKB) * 100.0
			stats["ram"] = float64(int(ramPercent*10)) / 10.0
			stats["memory_used"] = usedKB * 1024
			stats["memory_total"] = totalKB * 1024
			stats["memory_free"] = availKB * 1024
			return stats
		}
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	stats["memory_used"] = m.Alloc
	stats["memory_total"] = m.Sys
	if m.Sys > 0 {
		ramPercent := float64(m.Alloc) / float64(m.Sys) * 100.0
		stats["ram"] = float64(int(ramPercent*10)) / 10.0
	}
	return stats
}

func getCpuUsage() float64 {
	cpuState.mu.Lock()
	defer cpuState.mu.Unlock()

	now := time.Now()
	if now.Sub(cpuState.lastCheck) < 2*time.Second && cpuState.cachedVal > 0 {
		return cpuState.cachedVal
	}

	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0.0
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)[1:]
			var total, idle uint64
			for i, f := range fields {
				val, _ := strconv.ParseUint(f, 10, 64)
				total += val
				if i == 3 {
					idle = val
				}
			}

			if cpuState.lastTotal > 0 && total > cpuState.lastTotal {
				diffTotal := float64(total - cpuState.lastTotal)
				diffIdle := float64(idle - cpuState.lastIdle)
				pct := (diffTotal - diffIdle) / diffTotal * 100.0
				if pct < 0 {
					pct = 0
				}
				cpuState.cachedVal = float64(int(pct*10)) / 10.0
			}
			cpuState.lastTotal = total
			cpuState.lastIdle = idle
			cpuState.lastCheck = now
			return cpuState.cachedVal
		}
	}
	return 0.0
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]string{"error": message})
}

func resolveDataPath(customPath string) string {
	if customPath != "" {
		return customPath
	}
	if env := os.Getenv("DATA_PATH"); env != "" {
		return env
	}
	targetDirs := []string{"/mnt/hdd/alphared-streamer", "/mnt/hdd/tapo-streamer"}
	for _, targetDir := range targetDirs {
		if err := os.MkdirAll(targetDir, 0755); err == nil {
			testFile := filepath.Join(targetDir, ".write_test")
			if err := os.WriteFile(testFile, []byte("ok"), 0644); err == nil {
				_ = os.Remove(testFile)
				return filepath.Join(targetDir, "cameras.json")
			}
		}
	}
	return "./cameras.json"
}

func parseFolderDate(name string) (time.Time, error) {
	if t, err := time.Parse("02-01-2006", name); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", name)
}

// startStreamHealthWatchdog monitors YouTube stream health only (not record).
// If YouTube ingest noData/bad for 3 consecutive checks (90s) AND camera RTSP is unreachable:
//
//	-> StopStream(id) only, set StreamStatus='OFFLINE'
//	-> Do NOT restart. Do NOT call any YouTube API.
//
// If YouTube ingest noData but camera IS reachable: restart stream worker only.
// Record worker is NEVER touched by watchdog.
func startStreamHealthWatchdog(engine *StreamEngine, store *CameraStore, settingsStore *SettingsStore, ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	unhealthyCount := make(map[string]int)
	// Cooldown restart per kamera: cegah loop Stop/Start tiap 90 detik yang
	// menghabiskan kuota API dan membuat FFmpeg restart terus-menerus.
	lastRestart := make(map[string]time.Time)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		cams := store.GetAll()
		s := settingsStore.Get()

		var token string
		if s.YouTubeClientID != "" && s.YouTubeClientSecret != "" && s.YouTubeRefreshToken != "" {
			// Quota guard: skip all YouTube API calls if quota exceeded
			quotaMu.RLock()
			blocked := quotaExceededUntil.After(time.Now())
			quotaMu.RUnlock()
			if blocked {
				continue
			}

			t, err := getYouTubeAccessToken(s.YouTubeClientID, s.YouTubeClientSecret, s.YouTubeRefreshToken)
			if err == nil {
				token = t
			}
		}

		for _, cam := range cams {
			logs := engine.getLogs(cam.ID)

			// 0. Auto-recovery: pulihkan kamera jika RTSP sudah online kembali
			if cam.StreamStatus != "STREAMING" {
				unhealthyCount[cam.ID] = 0

				// Cek konektivitas RTSP
				if rtspReachable(cam.RTSPUrl) {
					// Jika statusnya OFFLINE, bersihkan status macet agar UI tahu kamera online
					if cam.StreamStatus == "OFFLINE" {
						store.SetStreamStatus(cam.ID, "STOPPED", "", 0)
					}

					// Jika AutoStart aktif, hidupkan kembali worker yang tertinggal
					if cam.AutoStart {
						if cam.RecordHDD && cam.RecordStatus != "RECORDING" {
							msg := fmt.Sprintf("[%s] [Watchdog] Kamera %s online kembali. Auto-start perekaman HDD...", time.Now().Format("15:04:05"), cam.Name)
							logs.Camera.Append(msg)
							log.Println(msg)
							_ = engine.StartRecord(cam.ID)
						}
						if cam.EnableYouTube && cam.StreamKey != "" {
							if last, ok := lastRestart[cam.ID]; !ok || time.Since(last) >= 2*time.Minute {
								lastRestart[cam.ID] = time.Now()
								msg := fmt.Sprintf("[%s] [Watchdog] Kamera %s online kembali. Auto-start YouTube Live...", time.Now().Format("15:04:05"), cam.Name)
								logs.YouTube.Append(msg)
								log.Println(msg)
								_ = engine.StartStream(cam.ID)
							}
						}
					}
				}
				continue
			}

			// Pastikan rekaman HDD juga tetap jalan jika AutoStart aktif
			if cam.AutoStart && cam.RecordHDD && cam.RecordStatus != "RECORDING" {
				if rtspReachable(cam.RTSPUrl) {
					msg := fmt.Sprintf("[%s] [Watchdog] Perekaman HDD untuk %s tidak aktif padahal stream berjalan. Auto-start perekaman...", time.Now().Format("15:04:05"), cam.Name)
					logs.Camera.Append(msg)
					log.Println(msg)
					_ = engine.StartRecord(cam.ID)
				}
			}

			// 1. Check stream worker session is alive
			engine.mu.Lock()
			workers, exists := engine.sessions[cam.ID]
			streamActive := exists && workers != nil && workers.stream != nil && workers.stream.cancel != nil
			engine.mu.Unlock()

			if !streamActive {
				// Stream worker died but status still STREAMING — restart stream only
				// (dengan cooldown 2 menit agar tidak restart beruntun).
				if last, ok := lastRestart[cam.ID]; ok && time.Since(last) < 2*time.Minute {
					continue
				}
				lastRestart[cam.ID] = time.Now()
				msg := fmt.Sprintf("[%s] [Watchdog] Stream worker kamera %s mati tidak terduga. Me-restart stream...", time.Now().Format("15:04:05"), cam.Name)
				logs.YouTube.Append(msg)
				log.Println(msg)
				_ = engine.StartStream(cam.ID)
				unhealthyCount[cam.ID] = 0
				continue
			}

			// 2. Check YouTube ingest health
			if cam.EnableYouTube && cam.StreamKey != "" && token != "" {
				streamStat, healthStat, err := getYouTubeStreamHealth(token, cam.StreamKey)
				if err != nil {
					continue
				}

				if streamStat != "active" || healthStat == "noData" {
					unhealthyCount[cam.ID]++
					logs.YouTube.Append(fmt.Sprintf("[%s] [Watchdog] Ingest stream status: %s, health: %s (deteksi %d/3)",
						time.Now().Format("15:04:05"), streamStat, healthStat, unhealthyCount[cam.ID]))

					if unhealthyCount[cam.ID] >= 3 { // ~90 detik terputus
						// Check if camera RTSP is reachable
						if !rtspReachable(cam.RTSPUrl) {
							// Camera offline — stop stream, set OFFLINE, do NOT restart
							msg := fmt.Sprintf("[%s] [Watchdog] Kamera offline >3 menit. YouTube Live dihentikan otomatis. Klik Mulai Live untuk memulai ulang.",
								time.Now().Format("15:04:05"))
							logs.YouTube.Append(msg)
							log.Println(msg)
							_ = engine.StopStream(cam.ID)
							store.SetStreamStatus(cam.ID, "OFFLINE", "Kamera offline >3 menit", 0)
							unhealthyCount[cam.ID] = 0
						} else {
							// Camera reachable but YouTube ingest bad — restart stream only,
							// dengan cooldown 5 menit agar tidak loop tiap 90 detik.
							if last, ok := lastRestart[cam.ID]; ok && time.Since(last) < 5*time.Minute {
								logs.YouTube.Append(fmt.Sprintf("[%s] [Watchdog] Ingest masih bermasalah, tapi restart ditahan (cooldown 5 menit) demi hemat kuota API.", time.Now().Format("15:04:05")))
								continue
							}
							lastRestart[cam.ID] = time.Now()
							msg := fmt.Sprintf("[%s] [Watchdog] YouTube ingest %s terputus selama 90s tapi kamera online. Me-restart stream...",
								time.Now().Format("15:04:05"), cam.Name)
							logs.YouTube.Append(msg)
							log.Println(msg)
							_ = engine.StopStream(cam.ID)
							time.Sleep(3 * time.Second)
							_ = engine.StartStream(cam.ID)
							unhealthyCount[cam.ID] = 0
						}
					}
				} else {
					unhealthyCount[cam.ID] = 0

					// Ingest aktif! Cek apakah ada siaran live yang AKTIF atau sudah COMPLETE
					targetStreamID, errStream := findYouTubeStreamID(token, cam.StreamKey)
					if errStream == nil {
						hasLiveBroadcast := false
						var readyBroadcastID string
						var readyBroadcastAge time.Duration

						listURL := "https://www.googleapis.com/youtube/v3/liveBroadcasts?part=id,snippet,status,contentDetails&mine=true&maxResults=20"
						reqList, _ := http.NewRequest("GET", listURL, nil)
						reqList.Header.Set("Authorization", "Bearer "+token)
						client := &http.Client{Timeout: 10 * time.Second}
						if resp, err := client.Do(reqList); err == nil {
							var bData struct {
								Items []struct {
									ID      string `json:"id"`
									Snippet struct {
										ScheduledStartTime string `json:"scheduledStartTime"`
									} `json:"snippet"`
									Status struct {
										LifeCycleStatus string `json:"lifeCycleStatus"`
									} `json:"status"`
									ContentDetails struct {
										BoundStreamID string `json:"boundStreamId"`
									} `json:"contentDetails"`
								} `json:"items"`
							}
							if json.NewDecoder(resp.Body).Decode(&bData) == nil {
								for _, it := range bData.Items {
									if it.ContentDetails.BoundStreamID == targetStreamID {
										if it.Status.LifeCycleStatus == "live" || it.Status.LifeCycleStatus == "liveStarting" {
											hasLiveBroadcast = true
											break
										}
										if it.Status.LifeCycleStatus == "ready" {
											readyBroadcastID = it.ID
											if tStart, errT := time.Parse(time.RFC3339, it.Snippet.ScheduledStartTime); errT == nil {
												readyBroadcastAge = time.Since(tStart)
											}
										}
									}
								}
							}
							resp.Body.Close()
						}

						// Jika ada siaran aktif yang sedang 'live', hapus 'ready' yang menggantung
						if hasLiveBroadcast && readyBroadcastID != "" {
							delURL := fmt.Sprintf("https://www.googleapis.com/youtube/v3/liveBroadcasts?id=%s", url.QueryEscape(readyBroadcastID))
							reqDel, _ := http.NewRequest("DELETE", delURL, nil)
							reqDel.Header.Set("Authorization", "Bearer "+token)
							_, _ = client.Do(reqDel)
						}
						if !hasLiveBroadcast && readyBroadcastID != "" && readyBroadcastAge > 90*time.Second {
							logs.YouTube.Append(fmt.Sprintf("[%s] [Watchdog] Broadcast %s stuck di status 'ready' (%v). Membersihkan & membuat ulang broadcast...",
								time.Now().Format("15:04:05"), readyBroadcastID, readyBroadcastAge.Round(time.Second)))
							delURL := fmt.Sprintf("https://www.googleapis.com/youtube/v3/liveBroadcasts?id=%s", url.QueryEscape(readyBroadcastID))
							reqDel, _ := http.NewRequest("DELETE", delURL, nil)
							reqDel.Header.Set("Authorization", "Bearer "+token)
							_, _ = client.Do(reqDel)
							readyBroadcastID = ""
						}

						// Jika data RTMP mengalir tapi tidak ada broadcast live — restart stream only
						if !hasLiveBroadcast && readyBroadcastID == "" {
							// Cooldown sama: cegah pembuatan broadcast berulang tiap 30 detik
							if last, ok := lastRestart[cam.ID]; ok && time.Since(last) < 15*time.Minute {
								logs.YouTube.Append(fmt.Sprintf("[%s] [Watchdog] Tidak ada broadcast live, tapi restart ditahan (cooldown 15 menit) demi hemat kuota API.", time.Now().Format("15:04:05")))
								continue
							}
							lastRestart[cam.ID] = time.Now()
							msg := fmt.Sprintf("[%s] [Watchdog] Siaran YouTube sebelumnya telah selesai/ditutup. Me-restart stream...", time.Now().Format("15:04:05"))
							logs.YouTube.Append(msg)
							log.Println(msg)

							_ = engine.StopStream(cam.ID)
							time.Sleep(3 * time.Second)
							_ = engine.StartStream(cam.ID)
							unhealthyCount[cam.ID] = 0
							continue
						}
					}
				}
			}
		}
	}
}

func startRetentionWorker(store *CameraStore, settingsStore *SettingsStore, ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	runCheck := func() {
		now := time.Now()
		cams := store.GetAll()
		currSettings := settingsStore.Get()

		for _, cam := range cams {
			baseDir := filepath.Join("/mnt/hdd/cctv", cam.ID)
			if cam.RecordHDD {
				_ = os.MkdirAll(filepath.Join(baseDir, now.Format("02-01-2006")), 0755)
			}

			retentionDays := cam.RetentionDays
			if retentionDays <= 0 {
				retentionDays = currSettings.DiskRetentionDays
				if retentionDays <= 0 {
					retentionDays = 7
				}
			}
			cutoff := now.AddDate(0, 0, -retentionDays)

			entries, err := os.ReadDir(baseDir)
			if err != nil {
				continue
			}

			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				folderDate, err := parseFolderDate(entry.Name())
				if err != nil {
					continue
				}
				if folderDate.Before(cutoff) {
					target := filepath.Join(baseDir, entry.Name())
					log.Printf("[Retention] Purging expired folder %s", target)
					_ = os.RemoveAll(target)
				}
			}
		}

		minFreeGB := float64(currSettings.DiskGuardMinGB)
		if minFreeGB <= 0 {
			minFreeGB = 10.0
		}

		for {
			_, _, freeGB, _, err := getDiskStats("/mnt/hdd")
			if err != nil || freeGB >= minFreeGB {
				break
			}

			type folderInfo struct {
				path string
				date time.Time
			}
			var allFolders []folderInfo

			for _, cam := range cams {
				baseDir := filepath.Join("/mnt/hdd/cctv", cam.ID)
				entries, err := os.ReadDir(baseDir)
				if err != nil {
					continue
				}
				for _, entry := range entries {
					if entry.IsDir() {
						if d, err := parseFolderDate(entry.Name()); err == nil {
							allFolders = append(allFolders, folderInfo{
								path: filepath.Join(baseDir, entry.Name()),
								date: d,
							})
						}
					}
				}
			}

			if len(allFolders) == 0 {
				break
			}

			sort.Slice(allFolders, func(i, j int) bool {
				return allFolders[i].date.Before(allFolders[j].date)
			})

			oldest := allFolders[0]
			log.Printf("[DiskGuard] Low free space (%.1f GB < 10 GB). Removing oldest folder %s", freeGB, oldest.path)
			if err := os.RemoveAll(oldest.path); err != nil {
				break
			}
		}
	}

	runCheck()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCheck()
		}
	}
}

type RecordingSegment struct {
	Filename  string  `json:"filename"`
	Segment   int     `json:"segment"`
	Path      string  `json:"path"`
	SizeBytes int64   `json:"size_bytes"`
	SizeMB    float64 `json:"size_mb"`
	StartTime string  `json:"start_time"`
	EndTime   string  `json:"end_time"`
	URL       string  `json:"url"`
}

func parseSegmentDetails(filename, dateStr string, modTime time.Time) (segment int, startTime, endTime string) {
	prefix := dateStr + "-"
	base := strings.TrimSuffix(filename, filepath.Ext(filename))

	if strings.HasPrefix(filename, prefix) {
		numStr := strings.TrimPrefix(base, prefix)
		if n, err := strconv.Atoi(numStr); err == nil {
			segment = n
			if !modTime.IsZero() {
				// Berkas berdurasi hingga 1 jam: jam mulai dihitung dari waktu modifikasi dikurangi 1 jam
				st := modTime.Add(-1 * time.Hour)
				startTime = st.Format("15:04")
				endTime = modTime.Format("15:04")
				return
			}
			startH := (n - 1) % 24
			endH := n % 24
			startTime = fmt.Sprintf("%02d:00", startH)
			endTime = fmt.Sprintf("%02d:00", endH)
			return
		}
	}

	// Fallback legacy HH-MM-SS
	t, err := time.Parse("15-04-05", base)
	if err == nil {
		startTime = t.Format("15:04")
		endTime = t.Add(15 * time.Minute).Format("15:04")
		return 0, startTime, endTime
	}

	if !modTime.IsZero() {
		startTime = modTime.Add(-1 * time.Hour).Format("15:04")
		endTime = modTime.Format("15:04")
		return 0, startTime, endTime
	}

	return 0, "--:--", "--:--"
}

func isValidCctvPath(p string) bool {
	if strings.Contains(p, "..") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	return strings.HasPrefix(clean, "/mnt/hdd/cctv/")
}

func main() {
	portFlag := flag.String("port", "", "Port HTTP server (default 8085 / env PORT)")
	dataFlag := flag.String("data", "", "Path file database JSON (default /mnt/hdd/alphared-streamer/cameras.json or ./cameras.json)")
	flag.Parse()

	port := "8085"
	if *portFlag != "" {
		port = *portFlag
	} else if envPort := os.Getenv("PORT"); envPort != "" {
		port = envPort
	}

	dataPath := resolveDataPath(*dataFlag)
	log.Printf("[Init] Database path: %s", dataPath)

	settingsPath := filepath.Join(filepath.Dir(dataPath), "settings.json")
	settingsStore, err := NewSettingsStore(settingsPath)
	if err != nil {
		log.Fatalf("[Error] Failed to initialize settings store: %v", err)
	}

	// YouTube credentials loaded from settingsStore (configured via UI or settings.json)
	store, err := NewCameraStore(dataPath)
	if err != nil {
		log.Fatalf("[Error] Failed to initialize camera store: %v", err)
	}

	engine := NewStreamEngine(store, settingsStore)

	ctxWorker, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go startRetentionWorker(store, settingsStore, ctxWorker)
	go startStreamHealthWatchdog(engine, store, settingsStore, ctxWorker)

	// AutoStart: pulihkan status masing-masing worker secara independen.
	// - Record: lanjut jika AutoStart ATAU sebelumnya memang sedang merekam.
	// - Stream: HANYA jika AutoStart. Jika sebelumnya dihentikan otomatis karena
	//   kamera offline, biarkan OFFLINE sampai user menekan "Mulai Live" manual.
	for _, cam := range store.GetAll() {
		startRec := cam.RecordHDD && (cam.AutoStart || cam.RecordStatus == "RECORDING")
		startStr := cam.EnableYouTube && cam.StreamKey != "" && cam.AutoStart

		if !startRec && !startStr {
			continue
		}
		log.Printf("[AutoStart] Camera %s (%s): record=%v stream=%v", cam.Name, cam.ID, startRec, startStr)

		if startRec {
			if err := engine.StartRecord(cam.ID); err != nil {
				log.Printf("[AutoStart] Error starting record for %s: %v", cam.ID, err)
			}
		}
		if startStr {
			if err := engine.StartStream(cam.ID); err != nil {
				log.Printf("[AutoStart] Error starting stream for %s: %v", cam.ID, err)
			}
		}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	mux.HandleFunc("GET /panduan", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(panduanMD)
	})

	mux.HandleFunc("GET /api/cameras", func(w http.ResponseWriter, r *http.Request) {
		cameras := store.GetAll()
		for i := range cameras {
			if cameras[i].Status == "LIVE" {
				cameras[i].Uptime = formatUptime(cameras[i].StartedAt)
			}
			if cameras[i].RecordStatus == "RECORDING" {
				cameras[i].RecordUptime = formatUptime(cameras[i].RecordStartedAt)
			}
			if cameras[i].StreamStatus == "STREAMING" {
				cameras[i].StreamUptime = formatUptime(cameras[i].StreamStartedAt)
			}
		}
		jsonResponse(w, http.StatusOK, cameras)
	})

	mux.HandleFunc("POST /api/cameras", func(w http.ResponseWriter, r *http.Request) {
		var payload Camera
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON body: "+err.Error())
			return
		}
		if strings.TrimSpace(payload.Name) == "" || strings.TrimSpace(payload.RTSPUrl) == "" {
			jsonError(w, http.StatusBadRequest, "Name and RTSPUrl are required")
			return
		}
		if payload.RTMPUrl == "" {
			payload.RTMPUrl = "rtmp://a.rtmp.youtube.com/live2"
		}
		if payload.RetentionDays <= 0 {
			payload.RetentionDays = 7
		}
		if !payload.EnableYouTube && !payload.RecordHDD {
			payload.EnableYouTube = true
		}
		created, err := store.Add(payload)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "Failed to save camera: "+err.Error())
			return
		}
		if created.AutoStart {
			_ = engine.Start(created.ID)
		}
		jsonResponse(w, http.StatusCreated, created)
	})

	mux.HandleFunc("PUT /api/cameras/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		existing, ok := store.Get(id)
		if !ok {
			jsonError(w, http.StatusNotFound, "Camera not found")
			return
		}
		var payload Camera
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON body: "+err.Error())
			return
		}
		existing.Name = payload.Name
		existing.RTSPUrl = payload.RTSPUrl
		existing.RTSPSubUrl = payload.RTSPSubUrl
		existing.RTMPUrl = payload.RTMPUrl
		existing.StreamKey = payload.StreamKey
		existing.YouTubeTitleTemplate = payload.YouTubeTitleTemplate
		existing.YouTubePlaylistID = payload.YouTubePlaylistID
		existing.LiveUrl = payload.LiveUrl
		existing.AutoStart = payload.AutoStart
		existing.EnableYouTube = payload.EnableYouTube
		existing.RecordHDD = payload.RecordHDD
		if payload.RetentionDays > 0 {
			existing.RetentionDays = payload.RetentionDays
		} else if existing.RetentionDays <= 0 {
			existing.RetentionDays = 7
		}

		// Restart hanya worker yang memang sedang aktif (jangan bangunkan YouTube
		// yang sengaja dimatikan / sedang OFFLINE).
		recordWasActive := existing.RecordStatus == "RECORDING"
		streamWasActive := existing.StreamStatus == "STREAMING"
		if err := store.Update(existing); err != nil {
			jsonError(w, http.StatusInternalServerError, "Failed to update camera: "+err.Error())
			return
		}

		if recordWasActive || streamWasActive {
			if recordWasActive {
				_ = engine.StopRecord(id)
			}
			if streamWasActive {
				_ = engine.StopStream(id)
			}
			time.Sleep(500 * time.Millisecond)
			if recordWasActive {
				_ = engine.StartRecord(id)
			}
			if streamWasActive {
				_ = engine.StartStream(id)
			}
		}
		jsonResponse(w, http.StatusOK, existing)
	})

	mux.HandleFunc("DELETE /api/cameras/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		_ = engine.Stop(id)
		if err := store.Delete(id); err != nil {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"message": "Camera deleted"})
	})

	// Backward-compat: start/stop both workers
	mux.HandleFunc("POST /api/cameras/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := engine.Start(id); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"message": "Stream started"})
	})

	mux.HandleFunc("POST /api/cameras/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := engine.Stop(id); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"message": "Stream stopped"})
	})

	// New independent worker endpoints
	mux.HandleFunc("POST /api/cameras/{id}/record/start", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := engine.StartRecord(id); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"message": "Recording started"})
	})

	mux.HandleFunc("POST /api/cameras/{id}/record/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := engine.StopRecord(id); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"message": "Recording stopped"})
	})

	mux.HandleFunc("POST /api/cameras/{id}/stream/start", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := engine.StartStream(id); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"message": "YouTube streaming started"})
	})

	mux.HandleFunc("POST /api/cameras/{id}/stream/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := engine.StopStream(id); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"message": "YouTube streaming stopped"})
	})

	mux.HandleFunc("GET /api/cameras/{id}/preview", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cam, ok := store.Get(id)
		if !ok {
			jsonError(w, http.StatusNotFound, "Camera not found")
			return
		}

		// Sama seperti /live: C200 hanya mengizinkan satu koneksi RTSP per stream,
		// dan perekam sudah memegang stream1. Snapshot WAJIB dari substream,
		// kalau tidak ffmpeg gagal konek dan UI harus menunggu timeout.
		rtspUrl := cam.RTSPSubUrl
		if rtspUrl == "" {
			rtspUrl = strings.Replace(cam.RTSPUrl, "/stream1", "/stream2", 1)
		}

		ctxSnapshot, cancelSnapshot := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancelSnapshot()

		cmd := exec.CommandContext(ctxSnapshot, "ffmpeg",
			"-nostdin",
			"-rtsp_transport", "tcp",
			"-fflags", "nobuffer",
			"-flags", "low_delay",
			"-i", rtspUrl,
			"-vframes", "1",
			"-q:v", "3",
			"-f", "image2",
			"-",
		)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "Failed to create stdout pipe: "+err.Error())
			return
		}

		if err := cmd.Start(); err != nil {
			jsonError(w, http.StatusInternalServerError, "Failed to start ffmpeg snapshot: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		_, copyErr := io.Copy(w, stdout)
		_ = cmd.Wait()

		if copyErr != nil && ctxSnapshot.Err() != nil {
			log.Printf("[Preview] Snapshot timeout for cam %s", id)
		}
	})

	// GET /api/cameras/{id}/live — passthrough H.264 mentah ke browser (fMP4).
	// Nol decode, nol encode di STB: byte RTSP disalin apa adanya ke fragmen MP4,
	// browser yang men-decode lewat MSE. Terukur 3.8% CPU vs 283% untuk MJPEG 25fps.
	mux.HandleFunc("GET /api/cameras/{id}/live", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cam, ok := store.Get(id)
		if !ok {
			jsonError(w, http.StatusNotFound, "Camera not found")
			return
		}

		// Kamera CCTV biasanya hanya mengizinkan satu koneksi RTSP per stream. Perekam HDD
		// sudah memegang stream1, jadi live WAJIB pakai substream. Kalau sub URL
		// belum diisi, turunkan /stream1 -> /stream2 supaya tidak bentrok.
		rtspUrl := cam.RTSPSubUrl
		if rtspUrl == "" {
			rtspUrl = strings.Replace(cam.RTSPUrl, "/stream1", "/stream2", 1)
		}

		flusher, canFlush := w.(http.Flusher)
		if !canFlush {
			jsonError(w, http.StatusInternalServerError, "Streaming not supported")
			return
		}

		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		// -c:v copy = tidak menyentuh pixel sama sekali. -an karena audio PCM alaw
		// tidak bisa dicopy ke MP4 tanpa transcode (dan browser tidak butuh audio CCTV).
		// frag_keyframe+empty_moov = header MP4 ditulis di awal supaya browser bisa
		// langsung mulai tanpa menunggu file selesai.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		cmd := exec.CommandContext(ctx, "ffmpeg",
			"-nostdin", "-loglevel", "error",
			"-rtsp_transport", "tcp",
			"-fflags", "nobuffer",
			"-flags", "low_delay",
			"-i", rtspUrl,
			"-c:v", "copy",
			"-an",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-frag_duration", "200000",
			"pipe:1",
		)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal membuka pipe: "+err.Error())
			return
		}
		cmd.Stderr = io.Discard

		if err := cmd.Start(); err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal menjalankan ffmpeg: "+err.Error())
			return
		}
		defer func() {
			cancel()
			_ = cmd.Wait()
		}()

		// Tulis dengan batas waktu. Tanpa ini, kalau browser berhenti membaca
		// (tab ditutup, HP tidur, jaringan putus) tetapi TCP belum putus,
		// w.Write blokir selamanya: r.Context() tidak pernah cancel, ffmpeg
		// hidup terus, dan goroutine-nya bocor. Terbukti di STB: satu ffmpeg
		// live bertahan 227 detik dengan Send-Q 578 KB menumpuk.
		rc := http.NewResponseController(w)

		buf := make([]byte, 32*1024)
		for {
			n, rerr := stdout.Read(buf)
			if n > 0 {
				_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				flusher.Flush()
			}
			if rerr != nil {
				return
			}
		}
	})

	mux.HandleFunc("GET /api/cameras/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		camLogs := engine.GetLogs(id, "camera")
		ytLogs := engine.GetLogs(id, "youtube")
		jsonResponse(w, http.StatusOK, map[string]string{
			"logs":         camLogs + "\n" + ytLogs, // backward-compatibility
			"camera_logs":  camLogs,
			"youtube_logs": ytLogs,
		})
	})

	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		s := settingsStore.Get()
		jsonResponse(w, http.StatusOK, s)
	})

	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var payload AppSettings
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
			return
		}
		updated, err := settingsStore.Update(func(s *AppSettings) {
			if strings.TrimSpace(payload.YouTubeClientID) != "" {
				s.YouTubeClientID = strings.TrimSpace(payload.YouTubeClientID)
			}
			if strings.TrimSpace(payload.YouTubeClientSecret) != "" {
				s.YouTubeClientSecret = strings.TrimSpace(payload.YouTubeClientSecret)
			}
			if payload.YouTubeRefreshToken != "" {
				s.YouTubeRefreshToken = strings.TrimSpace(payload.YouTubeRefreshToken)
			}
			if strings.TrimSpace(payload.YouTubePlaylistID) != "" {
				s.YouTubePlaylistID = strings.TrimSpace(payload.YouTubePlaylistID)
			}
			if payload.LiveDurationMinutes > 0 {
				s.LiveDurationMinutes = payload.LiveDurationMinutes
			}
			if payload.RecordSegmentSec > 0 {
				s.RecordSegmentSec = payload.RecordSegmentSec
			}
			if payload.DiskGuardMinGB > 0 {
				s.DiskGuardMinGB = payload.DiskGuardMinGB
			}
			if payload.DiskRetentionDays > 0 {
				s.DiskRetentionDays = payload.DiskRetentionDays
			}
		})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal menyimpan settings: "+err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, updated)
	})

	mux.HandleFunc("POST /api/settings/youtube-auth-url", func(w http.ResponseWriter, r *http.Request) {
		s := settingsStore.Get()
		if s.YouTubeClientID == "" {
			jsonError(w, http.StatusBadRequest, "YouTube Client ID belum diisi")
			return
		}
		// Redirect URI: kita pakai out-of-band atau redirect kembali ke host
		redirectURI := "urn:ietf:wg:oauth:2.0:oob"
		authURL := fmt.Sprintf("https://accounts.google.com/o/oauth2/v2/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=https://www.googleapis.com/auth/youtube&access_type=offline&prompt=consent",
			url.QueryEscape(s.YouTubeClientID), url.QueryEscape(redirectURI))
		jsonResponse(w, http.StatusOK, map[string]string{"url": authURL})
	})

	mux.HandleFunc("POST /api/settings/youtube-sync-playlist", func(w http.ResponseWriter, r *http.Request) {
		s := settingsStore.Get()
		if s.YouTubePlaylistID == "" {
			jsonError(w, http.StatusBadRequest, "ID Playlist YouTube belum diisi di Pengaturan")
			return
		}
		if s.YouTubeRefreshToken == "" {
			jsonError(w, http.StatusBadRequest, "Akun Google belum terhubung (Refresh token kosong)")
			return
		}

		token, err := getYouTubeAccessToken(s.YouTubeClientID, s.YouTubeClientSecret, s.YouTubeRefreshToken)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal mendapatkan access token: "+err.Error())
			return
		}

		// Ambil stream key dari kamera pertama atau default
		cams := store.GetAll()
		streamKey := ""
		camName := "CCTV Alphared"
		titleTpl := ""
		targetPlaylist := s.YouTubePlaylistID
		if len(cams) > 0 {
			streamKey = cams[0].StreamKey
			camName = cams[0].Name
			titleTpl = cams[0].YouTubeTitleTemplate
			if cams[0].YouTubePlaylistID != "" {
				targetPlaylist = cams[0].YouTubePlaylistID
			}
		}

		videoID, err := ensureYouTubeBroadcast(token, streamKey, camName, titleTpl, true)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal create/find live broadcast: "+err.Error())
			return
		}

		if err := addVideoToYouTubePlaylist(token, targetPlaylist, videoID); err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal menambahkan video ke playlist: "+err.Error())
			return
		}

		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"success":     true,
			"video_id":    videoID,
			"playlist_id": targetPlaylist,
			"message":     fmt.Sprintf("Sukses! Video siaran live (%s) berhasil dimasukkan ke playlist (%s).", videoID, targetPlaylist),
		})
	})

	mux.HandleFunc("POST /api/settings/youtube-auth-code", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
			jsonError(w, http.StatusBadRequest, "Kode otorisasi tidak boleh kosong")
			return
		}
		s := settingsStore.Get()
		if s.YouTubeClientID == "" || s.YouTubeClientSecret == "" {
			jsonError(w, http.StatusBadRequest, "Client ID dan Client Secret harus diisi terlebih dahulu")
			return
		}

		code := strings.TrimSpace(req.Code)
		if unescaped, err := url.QueryUnescape(code); err == nil && unescaped != "" {
			code = unescaped
		}

		form := url.Values{}
		form.Set("code", code)
		form.Set("client_id", s.YouTubeClientID)
		form.Set("client_secret", s.YouTubeClientSecret)
		form.Set("redirect_uri", "urn:ietf:wg:oauth:2.0:oob")
		form.Set("grant_type", "authorization_code")

		resp, err := http.PostForm("https://oauth2.googleapis.com/token", form)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal menghubungi Google OAuth: "+err.Error())
			return
		}
		defer resp.Body.Close()

		bodyBytes, _ := io.ReadAll(resp.Body)

		var tokenResp struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Error        string `json:"error"`
			ErrorDesc    string `json:"error_description"`
		}
		if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal membaca balasan token Google: "+string(bodyBytes))
			return
		}
		if tokenResp.Error != "" {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("Google Error (%s): %s", tokenResp.Error, tokenResp.ErrorDesc))
			return
		}
		if tokenResp.RefreshToken == "" {
			jsonError(w, http.StatusBadRequest, "Google tidak mengembalikan Refresh Token. Pastikan menghapus izin lama di Google Security atau gunakan prompt consent.")
			return
		}

		updated, err := settingsStore.Update(func(cfg *AppSettings) {
			cfg.YouTubeRefreshToken = tokenResp.RefreshToken
		})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "Gagal menyimpan token: "+err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"message":  "Berhasil menghubungkan akun YouTube!",
			"settings": updated,
		})
	})

	mux.HandleFunc("GET /api/system/stats", func(w http.ResponseWriter, r *http.Request) {
		stats := getSystemStats(engine.ActiveCount())
		jsonResponse(w, http.StatusOK, stats)
	})

	// GET /api/system/youtube-status — status jujur integrasi YouTube untuk panel UI.
	// Tanpa ini kartu "Kesehatan Siaran" hanya hiasan.
	mux.HandleFunc("GET /api/system/youtube-status", func(w http.ResponseWriter, r *http.Request) {
		s := settingsStore.Get()

		quotaMu.RLock()
		quotaBlocked := quotaExceededUntil.After(time.Now())
		var quotaResetAt string
		var quotaResetIn int
		if quotaBlocked {
			quotaResetAt = quotaExceededUntil.Format(time.RFC3339)
			quotaResetIn = int(time.Until(quotaExceededUntil).Seconds())
			if quotaResetIn < 0 {
				quotaResetIn = 0
			}
		}
		quotaMu.RUnlock()

		type guardInfo struct {
			StreamKeyPrefix string `json:"stream_key_prefix"`
			SecondsSince    int    `json:"seconds_since_create"`
			Active          bool   `json:"active"`
		}
		guards := make([]guardInfo, 0)
		lastCreateMu.Lock()
		for key, t := range lastBroadcastCreate {
			prefix := key
			if len(prefix) > 8 {
				prefix = prefix[:8] + "..."
			}
			guards = append(guards, guardInfo{
				StreamKeyPrefix: prefix,
				SecondsSince:    int(time.Since(t).Seconds()),
				Active:          time.Since(t) < 15*time.Minute,
			})
		}
		lastCreateMu.Unlock()

		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"connected":       s.YouTubeRefreshToken != "",
			"has_credentials": s.YouTubeClientID != "" && s.YouTubeClientSecret != "",
			"playlist_id":     s.YouTubePlaylistID,
			"quota_blocked":   quotaBlocked,
			"quota_reset_at":  quotaResetAt,
			"quota_reset_in":  quotaResetIn,
			"broadcast_guard": guards,
		})
	})

	// GET /api/system/storage/{id} — ringkasan pemakaian disk per kamera.
	mux.HandleFunc("GET /api/system/storage/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := store.Get(id); !ok {
			jsonError(w, http.StatusNotFound, "Camera not found")
			return
		}

		todayFolder := time.Now().Format("02-01-2006")
		todayDir := filepath.Join("/mnt/hdd/cctv", id, todayFolder)

		segCount := 0
		var todayBytes int64
		if entries, err := os.ReadDir(todayDir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".mkv") {
					continue
				}
				if info, err := e.Info(); err == nil {
					segCount++
					todayBytes += info.Size()
				}
			}
		}

		// Hitung total hari terekam + total ukuran seluruh arsip kamera ini.
		daysRecorded := 0
		var totalBytes int64
		if dayEntries, err := os.ReadDir(filepath.Join("/mnt/hdd/cctv", id)); err == nil {
			for _, d := range dayEntries {
				if !d.IsDir() {
					continue
				}
				if _, err := parseFolderDate(d.Name()); err != nil {
					continue
				}
				daysRecorded++
				files, err := os.ReadDir(filepath.Join("/mnt/hdd/cctv", id, d.Name()))
				if err != nil {
					continue
				}
				for _, f := range files {
					if info, err := f.Info(); err == nil && !f.IsDir() {
						totalBytes += info.Size()
					}
				}
			}
		}

		_, _, freeGB, _, _ := getDiskStats("/mnt/hdd")

		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"camera_id":          id,
			"date":               todayFolder,
			"today_segments":     segCount,
			"today_mb":           float64(int((float64(todayBytes)/(1024*1024))*10)) / 10,
			"days_recorded":      daysRecorded,
			"total_mb":           float64(int((float64(totalBytes)/(1024*1024))*10)) / 10,
			"disk_free_gb":       freeGB,
			"projected_full_day": "",
		})
	})

	mux.HandleFunc("GET /api/recordings/dates", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "Missing camera id parameter")
			return
		}
		camDir := filepath.Join("/mnt/hdd/cctv", filepath.Clean(id))
		entries, err := os.ReadDir(camDir)
		if err != nil {
			jsonResponse(w, http.StatusOK, map[string]interface{}{
				"camera_id": id,
				"dates":     []string{},
			})
			return
		}

		type dateEntry struct {
			raw string
			t   time.Time
		}
		var list []dateEntry

		for _, entry := range entries {
			if entry.IsDir() {
				if t, err := parseFolderDate(entry.Name()); err == nil {
					list = append(list, dateEntry{raw: entry.Name(), t: t})
				}
			}
		}

		sort.Slice(list, func(i, j int) bool {
			return list[i].t.After(list[j].t)
		})

		dates := make([]string, len(list))
		for i, de := range list {
			dates[i] = de.raw
		}

		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"camera_id": id,
			"dates":     dates,
		})
	})

	mux.HandleFunc("GET /api/cameras/{id}/recordings", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		date := r.URL.Query().Get("date")
		if date == "" {
			jsonError(w, http.StatusBadRequest, "Missing date parameter")
			return
		}
		dateDir := filepath.Join("/mnt/hdd/cctv", filepath.Clean(id), filepath.Clean(date))
		entries, err := os.ReadDir(dateDir)
		if err != nil {
			jsonResponse(w, http.StatusOK, map[string]interface{}{
				"camera_id": id,
				"date":      date,
				"segments":  []RecordingSegment{},
			})
			return
		}

		var segments []RecordingSegment
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".mkv") {
				info, err := entry.Info()
				if err != nil {
					continue
				}
				sizeBytes := info.Size()
				sizeMB := float64(int((float64(sizeBytes)/(1024*1024))*100)) / 100.0
				fullPath := filepath.ToSlash(filepath.Join(dateDir, entry.Name()))
				segNum, sTime, eTime := parseSegmentDetails(entry.Name(), date, info.ModTime())

				segments = append(segments, RecordingSegment{
					Filename:  entry.Name(),
					Segment:   segNum,
					Path:      fullPath,
					SizeBytes: sizeBytes,
					SizeMB:    sizeMB,
					StartTime: sTime,
					EndTime:   eTime,
					URL:       "/api/recordings/stream?path=" + fullPath,
				})
			}
		}

		sort.Slice(segments, func(i, j int) bool {
			if segments[i].Segment != segments[j].Segment && segments[i].Segment > 0 && segments[j].Segment > 0 {
				return segments[i].Segment < segments[j].Segment
			}
			return segments[i].Filename < segments[j].Filename
		})

		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"camera_id": id,
			"date":      date,
			"segments":  segments,
		})
	})

	mux.HandleFunc("POST /api/recordings/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
			return
		}

		deletedCount := 0
		var errs []string
		touchedDirs := make(map[string]bool)

		for _, p := range req.Paths {
			if !isValidCctvPath(p) {
				errs = append(errs, fmt.Sprintf("Forbidden or invalid path: %s", p))
				continue
			}

			cleanPath := filepath.Clean(p)
			if err := os.Remove(cleanPath); err != nil {
				if !os.IsNotExist(err) {
					errs = append(errs, fmt.Sprintf("Failed to delete %s: %v", p, err))
				}
			} else {
				deletedCount++
				touchedDirs[filepath.Dir(cleanPath)] = true
			}
		}

		for dir := range touchedDirs {
			if isValidCctvPath(dir) {
				entries, err := os.ReadDir(dir)
				if err == nil && len(entries) == 0 {
					_ = os.Remove(dir)
				}
			}
		}

		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"deleted_count": deletedCount,
			"errors":        errs,
		})
	})

	mux.HandleFunc("POST /api/recordings/download-zip", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="recordings.zip"`)

		zw := zip.NewWriter(w)
		defer zw.Close()

		for _, p := range req.Paths {
			if !isValidCctvPath(p) {
				continue
			}
			cleanPath := filepath.Clean(p)
			f, err := os.Open(cleanPath)
			if err != nil {
				continue
			}

			info, err := f.Stat()
			if err != nil {
				f.Close()
				continue
			}

			header, err := zip.FileInfoHeader(info)
			if err != nil {
				f.Close()
				continue
			}
			header.Name = filepath.Base(cleanPath)
			header.Method = zip.Store

			writer, err := zw.CreateHeader(header)
			if err != nil {
				f.Close()
				continue
			}

			_, _ = io.Copy(writer, f)
			f.Close()
		}
	})

	mux.HandleFunc("GET /api/recordings/stream", func(w http.ResponseWriter, r *http.Request) {
		reqPath := r.URL.Query().Get("path")
		if reqPath == "" {
			http.Error(w, "Missing path parameter", http.StatusBadRequest)
			return
		}

		if !isValidCctvPath(reqPath) {
			http.Error(w, "Forbidden: invalid path traversal", http.StatusForbidden)
			return
		}

		cleanPath := filepath.Clean(reqPath)
		if _, err := os.Stat(cleanPath); os.IsNotExist(err) {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "video/x-matroska")
		if r.URL.Query().Get("download") == "1" {
			filename := filepath.Base(cleanPath)
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		}

		http.ServeFile(w, r, cleanPath)
	})

	mux.HandleFunc("GET /privacy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Privacy Policy</title></head><body style="font-family:sans-serif;max-width:800px;margin:40px auto;padding:0 20px;line-height:1.6"><h2>Privacy Policy</h2><p>This personal CCTV application uses the YouTube Data API v3 solely to manage live broadcast streams and automated CCTV playlist backups to the authenticated user's YouTube account. No personal user data is stored externally, sold, or shared with third parties.</p></body></html>`))
	})

	mux.HandleFunc("GET /terms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Terms of Service</title></head><body style="font-family:sans-serif;max-width:800px;margin:40px auto;padding:0 20px;line-height:1.6"><h2>Terms of Service</h2><p>This application is provided strictly for private home surveillance and personal live streaming purposes on an as-is basis.</p></body></html>`))
	})

	// ── Basic auth ────────────────────────────────────────────────────────
	// UI ini terbuka ke internet lewat Cloudflare Tunnel dan menguasai kamera,
	// rekaman, kredensial RTSP, dan stream key YouTube. Tanpa ini siapa pun yang
	// tahu URL bisa menonton, mengunduh, dan menghapus rekaman.
	// Kredensial dibaca dari environment supaya bisa diganti tanpa rebuild:
	//   NVR_USER / NVR_PASS pada unit systemd.
	authUser := envOr("NVR_USER", "admin")
	authPass := envOr("NVR_PASS", "")
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bypass basic auth untuk policy publik (Google OAuth requirement)
		if r.URL.Path == "/privacy" || r.URL.Path == "/terms" {
			mux.ServeHTTP(w, r)
			return
		}

		u, p, ok := r.BasicAuth()
		// Bandingkan keduanya, dan jangan berhenti di kegagalan pertama:
		// perbandingan konstan-waktu supaya panjang/isi tidak bocor lewat timing.
		uOK := subtle.ConstantTimeCompare([]byte(u), []byte(authUser)) == 1
		pOK := subtle.ConstantTimeCompare([]byte(p), []byte(authPass)) == 1
		if !ok || !uOK || !pOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Alphared NVR", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      authed,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // unlimited — /api/recordings/stream serves large video files
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[Server] alphared-streamer listening on http://0.0.0.0:%s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Server] Listen error: %v", err)
		}
	}()

	<-stopChan
	log.Println("[Shutdown] Stopping active streams and shutting down...")

	cancelWorker()
	engine.StopAll()

	ctxShutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(ctxShutdown); err != nil {
		log.Printf("[Shutdown] Server shutdown error: %v", err)
	}
	log.Println("[Shutdown] Done.")
}

// envOr mengembalikan nilai environment, atau def kalau kosong.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
