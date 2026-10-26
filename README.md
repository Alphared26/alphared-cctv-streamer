# Alphared NVR & YouTube Live Streamer

NVR (Network Video Recorder) mandiri super hemat resource berbasis Go, dirancang khusus untuk berjalan 24/7 di perangkat hemat daya berspesifikasi rendah seperti STB Linux (ZTE B860H, Fiberhome HG680P, X96 Mini ber-Armbian / Ubuntu Server) maupun Raspberry Pi.

Sistem ini merekam feed CCTV RTSP langsung ke harddisk eksternal sekaligus menyiarkannya non-stop (24/7 live streaming) ke YouTube secara otomatis tanpa memerlukan kartu MicroSD di kamera IP dan tanpa membebani CPU, berkat arsitektur **Zero-Transcode Video Passthrough (`-c:v copy`)**.

---

## Daftar Isi
- [Latar Belakang & Arsitektur](#latar-belakang--arsitektur)
- [Fitur Utama](#fitur-utama)
- [Persyaratan Sistem](#persyaratan-sistem)
- [Panduan Instalasi di STB (Armbian / Ubuntu)](#panduan-instalasi-di-stb-armbian--ubuntu)
  - [1. Instalasi Dependensi](#1-instalasi-dependensi)
  - [2. Menyiapkan Penyimpanan Eksternal (HDD/SSD)](#2-menyiapkan-penyimpanan-eksternal-hddssd)
  - [3. Download & Build Binary](#3-download--build-binary)
  - [4. Konfigurasi Systemd Service (Auto-Start)](#4-konfigurasi-systemd-service-auto-start)
- [Panduan Lengkap Setup Google Cloud OAuth 2.0 & YouTube API v3](#panduan-lengkap-setup-google-cloud-oauth-20--youtube-api-v3)
  - [1. Buat Project di Google Cloud Console](#1-buat-project-di-google-cloud-console)
  - [2. Aktifkan YouTube Data API v3](#2-aktifkan-youtube-data-api-v3)
  - [3. Konfigurasi OAuth Consent Screen](#3-konfigurasi-oauth-consent-screen)
  - [4. Menghindari Refresh Token Kadaluwarsa Tiap 7 Hari (Penting!)](#4-menghindari-refresh-token-kadaluwarsa-tiap-7-hari-penting)
  - [5. Buat OAuth 2.0 Client Credentials](#5-buat-oauth-20-client-credentials)
  - [6. Otorisasi Akun YouTube di Web UI](#6-otorisasi-akun-youtube-di-web-ui)
  - [7. Rotasi Broadcast & Sinkronisasi Playlist](#7-rotasi-broadcast--sinkronisasi-playlist)
- [Struktur Konfigurasi](#struktur-konfigurasi)
  - [cameras.json](#camerasjson)
  - [settings.json](#settingsjson)
- [Operasional & Troubleshooting](#operasional--troubleshooting)
  - [1. Error `invalid_grant` / Token Kadaluwarsa](#1-error-invalid_grant--token-kadaluwarsa)
  - [2. Batasan Kuota API YouTube (10.000 Units/Hari)](#2-batasan-kuota-api-youtube-10000-unitshari)
  - [3. Kamera RTSP Offline / Koneksi Putus](#3-kamera-rtsp-offline--koneksi-putus)
  - [4. Harddisk Penuh & Retensi Otomatis](#4-harddisk-penuh--retensi-otomatis)
- [Lisensi](#lisensi)

---

## Latar Belakang & Arsitektur

Banyak CCTV rumahan (IP Cam) mengandalkan penyimpanan MicroSD lokal yang rentan rusak akibat siklus tulis terus-menerus, atau langganan cloud berbayar. Mengubah PC desktop menjadi NVR memakan daya listrik besar (30W-100W+). 

STB Android bekas (Amlogic S905X / RAM 1GB-2GB) yang dipasangi Armbian hanya memakan daya 4W-7W, namun prosesor ARM-nya tidak mampu melakukan transcode video multi-channel.

**Solusi Alphared:**
1. **Zero-Transcoding:** Stream H.264/H.265 dan audio AAC dari RTSP dipindahkan langsung menggunakan container packaging tanpa re-encode (`-c:v copy -c:a copy` atau re-encode audio ultra-ringan AAC `-c:a aac` bila audio sumber PCM). Konsumsi RAM < 100MB dan beban CPU rata-rata < 10%.
2. **Dual FFmpeg Pipeline:** 
   - Engine 1: Menyimpan rekaman segmentasi berkala (misal 15 menit per file `.mp4`) ke direktori lokal HDD.
   - Engine 2: Mendorong feed video ke YouTube Live Ingestion Server via protokol RTMP (`rtmp://a.rtmp.youtube.com/live2`).
3. **Web-Native fMP4 MSE Monitoring:** Live preview kamera diakses via antarmuka web modern langsung menggunakan Media Source Extensions (fMP4) tanpa plugin pihak ketiga.

---

## Fitur Utama

- **Ultra-Low Memory Footprint:** Konsumsi memori runtime Go hanya ~35MB - 90MB, optimal untuk perangkat RAM 1GB.
- **Dual Engine Recording & Streaming:** Perekaman lokal dan live streaming YouTube berjalan pada subprocess independen; kegagalan stream internet tidak menghentikan perekaman lokal.
- **Auto-Recovery & Resilient Watchdog:** Jika koneksi internet terputus (misalnya router restart otomatis jam 3 pagi), watchdog secara cerdas mendeteksi kegagalan pipa RTMP, menahan siklus reconnect, dan memulihkan sesi stream tanpa merusak status NVR.
- **YouTube Lifecycle Automation:** Otomatis membuat siaran YouTube Live baru jika siaran lama berakhir, mengaitkan Stream Key, menyetel visibilitas (Public / Unlisted / Private), dan memasukkannya ke YouTube Playlist yang ditentukan.
- **Linear Dark Modern Web UI:** Tampilan web responsif berbasis dark theme yang nyaman dibuka melalui layar smartphone maupun monitor PC desktop.
- **Automatic Retention Management:** Penghapusan berkas rekaman tertua secara berkala saat kapasitas partisi HDD mencapai ambang batas yang ditentukan.

---

## Persyaratan Sistem

- **Hardware:**
  - Single Board Computer / STB Linux (ZTE B860H, Fiberhome HG680P, X96, Raspberry Pi 3/4/5, Thin Client x86).
  - RAM: Minimal 512MB (Rekomendasi: 1GB atau lebih).
  - Penyimpanan Sistem: MicroSD / eMMC 8GB+.
  - Penyimpanan Rekaman: Harddisk Eksternal / SSD USB 2.0/3.0 dengan adaptor daya terpisah jika diperlukan.
- **Sistem Operasi:** Armbian 22+, Ubuntu Server 20.04/22.04/24.04 LTS, Debian 11/12 (arm64, armhf, atau amd64).
- **Kamera:** IP Camera yang mendukung RTSP stream (H.264/AAC atau H.264/G.711).

---

## Panduan Instalasi di STB (Armbian / Ubuntu)

### 1. Instalasi Dependensi

Buka terminal SSH STB Anda dan pasang paket yang dibutuhkan:

```bash
sudo apt update && sudo apt install -y ffmpeg git curl wget jq
```

Pastikan FFmpeg terpasang dengan benar:
```bash
ffmpeg -version
```

*(Opsional)* Jika Anda ingin mengompilasi dari source code Go langsung di STB:
```bash
sudo apt install -y golang-go
go version
```

---

### 2. Menyiapkan Penyimpanan Eksternal (HDD/SSD)

Hubungkan HDD eksternal ke port USB STB. Cek UUID partisi penyimpanan:

```bash
sudo blkid
```

Buat direktori mount point dan pasang disk secara permanen:

```bash
sudo mkdir -p /mnt/hdd
```

Tambahkan entri ke `/etc/fstab` (ganti `UUID_ANDA_DISINI` dan tipe filesystem sesuai disk Anda, misal `ext4`):

```bash
sudo nano /etc/fstab
```

Tambahkan baris berikut di akhir file:
```text
UUID=UUID_ANDA_DISINI /mnt/hdd ext4 defaults,nofail,noatime 0 2
```

Terapkan mount dan siapkan direktori kerja aplikasi:

```bash
sudo mount -a
sudo mkdir -p /mnt/hdd/alphared-streamer/recordings
sudo mkdir -p /mnt/hdd/alphared-streamer/config
sudo chown -R $USER:$USER /mnt/hdd/alphared-streamer
```

---

### 3. Download & Build Binary

Masuk ke folder kerja dan clone repositori:

```bash
cd /mnt/hdd/alphared-streamer
git clone https://github.com/Alphared26/alphared-cctv-streamer.git .
```

Kompilasi aplikasi menjadi satu file binary executable:

```bash
go build -ldflags="-s -w" -o alphared-streamer main.go
chmod +x alphared-streamer
```

---

### 4. Konfigurasi Systemd Service (Auto-Start)

Agar aplikasi otomatis berjalan saat STB menyala atau reboot, buat service systemd:

```bash
sudo nano /etc/systemd/system/alphared-streamer.service
```

Tempelkan konfigurasi berikut:

```ini
[Unit]
Description=Alphared NVR & YouTube Live Streamer
After=network-online.target local-fs.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=/mnt/hdd/alphared-streamer
ExecStart=/mnt/hdd/alphared-streamer/alphared-streamer
Restart=always
RestartSec=5
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

Reload daemon dan aktifkan service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable alphared-streamer.service
sudo systemctl start alphared-streamer.service
```

Untuk memantau log aplikasi secara realtime:
```bash
journalctl -u alphared-streamer.service -f
```

---

## Panduan Lengkap Setup Google Cloud OAuth 2.0 & YouTube API v3

Untuk menyiarkan feed ke YouTube Live secara otomatis tanpa input manual setiap hari, aplikasi memanfaatkan **YouTube Data API v3**. Ikuti instruksi ini secara seksama agar token tidak kadaluwarsa.

---

### 1. Buat Project di Google Cloud Console

1. Buka browser dan login ke [Google Cloud Console](https://console.cloud.google.com/).
2. Di pojok kiri atas, klik dropdown project dan pilih **New Project**.
3. Isi **Project Name** (contoh: `Alphared-NVR-Streamer`).
4. Klik **Create** dan tunggu hingga project selesai dibuat, lalu pilih project tersebut.

---

### 2. Aktifkan YouTube Data API v3

1. Masuk ke menu **APIs & Services** > **Library**.
2. Di kolom pencarian, ketik `YouTube Data API v3`.
3. Klik pada kartu **YouTube Data API v3**, lalu klik tombol **Enable**.

---

### 3. Konfigurasi OAuth Consent Screen

1. Buka menu **APIs & Services** > **OAuth consent screen**.
2. Pilih User Type: **External** (pilihan ini memungkinkan akun Google pribadi Anda menggunakannya).
3. Klik **Create**.
4. **Isian Informasi Aplikasi:**
   - **App name:** `Alphared Streamer`
   - **User support email:** Pilih alamat email Google Anda.
   - **Developer contact information:** Masukkan alamat email Google Anda.
   - **PENTING: Biarkan Logo, App domain (Application home page, Privacy policy, Terms of service) KOSONG.**
     > *Mengapa?* Jika Anda mengunggah logo atau mengisi tautan domain sembarangan, Google Cloud akan mewajibkan proses "App Verification" resmi yang membutuhkan dokumen badan usaha dan waktu verifikasi berhari-hari. Membiarkannya kosong memungkinkan aplikasi berjalan untuk kebutuhan pribadi secara instan.
5. Klik **Save and Continue**.
6. **Scopes:**
   - Klik **Add or Remove Scopes**.
   - Masukkan / centang scope:
     `https://www.googleapis.com/auth/youtube` (Manage your YouTube account).
   - Klik **Update**, lalu klik **Save and Continue**.
7. **Test Users:**
   - Tambahkan alamat email akun Google / YouTube yang akan digunakan untuk live streaming.
   - Klik **Save and Continue**.

---

### 4. Menghindari Refresh Token Kadaluwarsa Tiap 7 Hari (Penting!)

> **PERHATIAN KHUSUS GOOGLE OAUTH SECURITY POLICY:**
> Jika status OAuth Consent Screen dibiarkan dalam status **"Testing"**, semua Refresh Token yang diterbitkan Google Cloud **akan hangus otomatis setiap 7 hari**. Ini akan menyebabkan live stream terhenti setiap minggu dengan error `invalid_grant`.

**Solusi Permanen:**
1. Kembali ke tab **OAuth consent screen**.
2. Di bawah judul **Publishing status**, perhatikan status yang bertuliskan **Testing**.
3. Klik tombol **PUBLISH APP**, lalu konfirmasi dengan menekan **Confirm**.
4. Status project sekarang berubah menjadi **In production** (atau *Needs Verification*).
5. **Abaikan peringatan verifikasi Google.** Karena aplikasi ini hanya Anda gunakan sendiri, Anda tidak perlu mengajukan verifikasi ke Google. Saat login nanti, Google hanya akan menampilkan peringatan layar kuning *"Google hasn't verified this app"*. Cukup klik **Advanced** > **Go to Alphared Streamer (unsafe)** untuk menyetujuinya sekali saja. Refresh Token kini berlaku permanen!

---

### 5. Buat OAuth 2.0 Client Credentials

1. Buka menu **APIs & Services** > **Credentials**.
2. Klik **Create Credentials** di bagian atas, lalu pilih **OAuth client ID**.
3. Pilih **Application type**: **Web application**.
4. Masukkan **Name**: `Alphared STB Client`.
5. Di bagian **Authorized redirect URIs**, klik **Add URI**:
   - Masukkan: `http://<IP_STB_ANDA>:8080/api/auth/youtube/callback`
   - Masukkan juga alternatif: `http://localhost:8080/api/auth/youtube/callback`
   *(Ganti `<IP_STB_ANDA>` dengan alamat IP lokal STB Anda, misalnya `http://192.168.1.50:8080/api/auth/youtube/callback`)*.
6. Klik **Create**.
7. Salin nilai **Client ID** dan **Client Secret** yang muncul ke dalam file `settings.json`.

---

### 6. Otorisasi Akun YouTube di Web UI

1. Pastikan service Alphared NVR sudah berjalan.
2. Buka browser melalui komputer atau ponsel yang berada di jaringan WiFi/LAN yang sama:
   `http://<IP_STB_ANDA>:8080`
3. Masuk ke menu **Settings** > **YouTube Integration**.
4. Masukkan **Client ID** dan **Client Secret** jika belum diisi di `settings.json`, lalu klik **Save**.
5. Klik tombol **Authorize with YouTube**.
6. Anda akan diarahkan ke halaman login Google:
   - Pilih akun Google pemilik channel YouTube Anda.
   - Jika muncul peringatan *"Google hasn't verified this app"*, klik tautan **Advanced** di pojok kiri bawah, lalu klik **Go to Alphared Streamer (unsafe)**.
   - Centang izin akses untuk mengelola channel YouTube.
   - Klik **Continue**.
7. Browser akan dialihkan kembali ke Web UI Alphared dengan status **Authorized (Active)**. Token refresh telah tersimpan dengan aman di STB.

---

### 7. Rotasi Broadcast & Sinkronisasi Playlist

YouTube membatasi durasi satu sesi siaran live stream maksimal 24 - 36 jam agar arsip video dapat disimpan secara utuh ke channel.

Alphared mengatasi ini dengan arsitektur **Auto-Rotation**:
- **Automatic Broadcast Creator:** NVR otomatis membuat broadcast live baru setiap 24 jam (dapat dikonfigurasi).
- **Auto-Playlist Synchronization:** Setiap broadcast baru otomatis dimasukkan ke dalam YouTube Playlist yang Anda tentukan (misalnya playlist `"Arsip CCTV Halaman Depan"`). Penonton atau pemilik rumah cukup membuka satu tautan playlist untuk melihat siaran langsung terkini maupun rekaman hari-hari sebelumnya.

---

## Struktur Konfigurasi

Semua konfigurasi disimpan dalam format JSON di direktori aplikasi.

### cameras.json

Menyimpan daftar kamera pengawas yang terhubung:

```json
[
  {
    "id": "cam-depan",
    "name": "Kamera Depan",
    "enabled": true,
    "rtsp_url": "rtsp://admin:password123@192.168.1.100:554/stream1",
    "record_local": true,
    "segment_minutes": 15,
    "youtube_live": {
      "enabled": true,
      "privacy_status": "unlisted",
      "playlist_id": "PLxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
      "title_template": "Live CCTV Depan Rumah - {{date}}",
      "description": "Siaran langsung pengawasan rumah 24/7 non-stop.",
      "auto_rotate_hours": 24
    }
  },
  {
    "id": "cam-belakang",
    "name": "Kamera Belakang",
    "enabled": true,
    "rtsp_url": "rtsp://admin:password123@192.168.1.101:554/h264Preview_01_main",
    "record_local": true,
    "segment_minutes": 15,
    "youtube_live": {
      "enabled": false
    }
  }
]
```

**Penjelasan Parameter `cameras.json`:**
- `id`: Identitas unik kamera (string tanpa spasi).
- `rtsp_url`: URL RTSP lengkap feed kamera beserta user dan password.
- `record_local`: `true` untuk menyimpan berkas video rekaman ke harddisk STB.
- `segment_minutes`: Durasi per potongan file video rekaman lokal dalam menit.
- `youtube_live.enabled`: `true` jika kamera ini akan disiarkan ke YouTube Live.
- `youtube_live.privacy_status`: Visibilitas video (`public`, `unlisted`, atau `private`).
- `youtube_live.playlist_id`: ID Playlist YouTube tujuan penambahan siaran live secara otomatis.
- `youtube_live.title_template`: Pola judul siaran. Tag `{{date}}` akan diubah menjadi tanggal otomatis.
- `youtube_live.auto_rotate_hours`: Interval pergantian sesi siaran baru (rekomendasi: 12 atau 24 jam).

---

### settings.json

Menyimpan konfigurasi server lokal, batas disk, dan kredensial API:

```json
{
  "server": {
    "port": 8080,
    "storage_path": "/mnt/hdd/alphared-streamer/recordings",
    "disk_max_usage_percent": 90,
    "retention_days": 14
  },
  "youtube_oauth": {
    "client_id": "xxxxxx.apps.googleusercontent.com",
    "client_secret": "GOCSPX-xxxxxxxxxxxxxxxxxxxx",
    "redirect_url": "http://192.168.1.50:8080/api/auth/youtube/callback",
    "refresh_token": ""
  },
  "ffmpeg": {
    "reconnect_delay_seconds": 5,
    "probe_size": "32M",
    "analyzeduration": "32M"
  }
}
```

**Penjelasan Parameter `settings.json`:**
- `server.port`: Port web server untuk mengakses dashboard NVR.
- `server.storage_path`: Lokasi absolut folder tempat file rekaman disimpan di harddisk.
- `server.disk_max_usage_percent`: Batas maksimal penggunaan partisi disk sebelum sistem retensi otomatis menghapus video paling lama.
- `server.retention_days`: Jumlah hari penyimpanan file lokal sebelum ditandai kedaluwarsa.
- `youtube_oauth.client_id`: Client ID dari Google Cloud Console.
- `youtube_oauth.client_secret`: Client Secret dari Google Cloud Console.
- `youtube_oauth.refresh_token`: Terisi otomatis oleh sistem setelah otorisasi Web UI berhasil.
- `ffmpeg.reconnect_delay_seconds`: Jeda waktu tunggu sebelum mencoba menyambungkan ulang RTSP jika terjadi kegagalan jaringan.

---

## Operasional & Troubleshooting

### 1. Error `invalid_grant` / Token Kadaluwarsa

- **Gejala:** Stream YouTube berhenti setelah 7 hari berjalan. Log journalctl mencatat `oauth2: cannot fetch token: 400 Bad Request: "error": "invalid_grant"`.
- **Penyebab:** Status aplikasi di Google Cloud Console masih berstatus **"Testing"**. Token pada mode testing otomatis dicabut oleh Google setelah 7 hari.
- **Solusi:**
  1. Masuk ke [Google Cloud Console](https://console.cloud.google.com/) > **OAuth consent screen**.
  2. Klik tombol **Publish App** untuk beralih ke status **In production**.
  3. Buka Web UI Alphared, klik **Re-authorize YouTube**, dan lakukan login ulang satu kali. Token Anda tidak akan hangus lagi.

---

### 2. Batasan Kuota API YouTube (10.000 Units/Hari)

- **Gejala:** Muncul error `quotaExceeded` (403 Forbidden) di log aplikasi.
- **Penyebab:** Google memberikan jatah kuota gratis sebesar 10.000 unit per hari untuk setiap project YouTube Data API v3. 
  - Operasi membuat broadcast (`liveBroadcasts.insert`) = 50 unit.
  - Mengikat stream (`liveBroadcasts.bind`) = 50 unit.
  - Memasukkan ke playlist (`playlistItems.insert`) = 50 unit.
- **Solusi:**
  - Jangan menyetel `auto_rotate_hours` terlalu pendek (misalnya tiap 1 jam). Gunakan nilai `24` jam agar satu kamera hanya mengonsumsi ~150 unit kuota per hari.
  - Alphared dilengkapi caching status stream lokal sehingga tidak melakukan polling query API berlebihan.

---

### 3. Kamera RTSP Offline / Koneksi Putus

- **Gejala:** Perekaman berhenti, status kamera di Web UI berubah menjadi warna merah (Offline).
- **Penyebab:** Kamera reboot berkala, kabel LAN/WiFi kamera bermasalah, atau router DHCP mengubah alamat IP kamera.
- **Solusi:**
  1. Kunci IP kamera menggunakan fitur **DHCP Static IP / MAC Binding** pada router Anda.
  2. Engine FFmpeg Alphared dilengkapi parameter `-reconnect 1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 10` yang akan mencoba tersambung kembali secara otomatis saat kamera menyala kembali.

---

### 4. Harddisk Penuh & Retensi Otomatis

- **Gejala:** STB menjadi lambat, proses perekaman FFmpeg keluar dengan pesan `No space left on device`.
- **Penyebab:** Piringan harddisk penuh karena batas retensi belum tercapai atau volume rekaman melebihi estimasi kapasitas.
- **Solusi:**
  - Pastikan nilai `disk_max_usage_percent` disetel ke angka aman (misal `85` atau `90`).
  - Worker retensi Alphared berjalan setiap 30 menit. Jika penggunaan disk melebihi batas tersebut, direktori rekaman tertua akan dihapus seketika hingga kapasitas berada di bawah ambang batas aman.

---

## Lisensi

Proyek ini dirilis di bawah lisensi [MIT License](LICENSE). Bebas digunakan, dimodifikasi, dan didistribusikan untuk keperluan pribadi maupun komersial.
