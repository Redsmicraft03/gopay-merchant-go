# gopay-merchant-go

Robot pengecek pembayaran GoBiz + penghubung settlement untuk topup
Talent Tools (mitra-tools). Satu perintah menjalankan semuanya:
server poller, pencocokan pembayaran, dan wire settlement — dalam
**satu proses**.

Cara kerja singkat: customer membayar order topup lewat QRIS GoPay
→ robot ini mengecek mutasi GoBiz → kalau nominalnya cocok persis
dengan total order (harga + kode unik MDR + fee), robot mengusulkan
settlement ke backend mitra-tools → backend mengkredit saldo talent.
Robot ini **tidak pernah** mengkredit saldo sendiri dan **tidak
pernah** mengakses database mitra-tools langsung.

> **Peringatan jujur:** koneksi GoBiz di sini **meniru dashboard web
> GoBiz, bukan API partner resmi** Meta/GoBiz. Pakai akun merchant
> GoPay sendiri, dan jaga file sesi (`.gobiz-session.json`) seperti
> password — jangan dibagikan atau di-commit.

## Prasyarat

- Go **>= 1.26** (`go version` untuk mengecek).
- Backend mitra-tools jalan (default `http://localhost:8080`).
- Akun GoBiz/GoPay merchant + nomor HP yang bisa menerima OTP
  (untuk login pertama).
- Postgres (opsional tapi disarankan) — tanpa itu, klaim dan
  reservasi hanya tersimpan di memori dan **hilang saat restart**.

## Mulai cepat (5 langkah)

```powershell
# 1. Masuk folder proyek
cd G:\KafeinArt\gopay-merchant-go

# 2. Salin contoh konfigurasi, lalu isi nilai yang wajib
copy .env.example .env
# Wajib diisi di .env: BRIDGE_INTERNAL_TOKEN (token internal mitra-tools),
# SOURCE=gobiz, dan DATABASE_URL kalau pakai Postgres.
# Nilai lain bisa dibiarkan default dulu.

# 3. Login GoBiz (sekali saja; HP + OTP diminta di terminal, tidak disimpan)
go run ./cmd/login

# 4. Jalankan server (satu proses: server + wire loop)
go run ./cmd/server

# 5. Cek di browser/terminal
#    http://127.0.0.1:3101/health
#    http://127.0.0.1:3101/        (dashboard)
```

Kalau langkah 4 menampilkan `Wire loop AKTIF di proses ini`, robot
sudah bekerja: setiap ada order PENDING, wire memeriksa mutasi GoBiz
tiap 3 detik dan mengusulkan settlement yang cocok.

## Perintah yang tersedia

Jalankan semuanya **dari folder proyek** (file `.env`, sesi,
`web/ui.html`, dan `data/` dibaca relatif dari folder ini).

```powershell
# Server + wire loop (cara utama, 1 perintah, 1 terminal)
go run ./cmd/server

# Build binary dulu, baru jalankan
go build -o gopay-merchant-go-server.exe ./cmd/server
.\gopay-merchant-go-server.exe

# Login GoBiz (interaktif: HP, lalu OTP)
go run ./cmd/login

# Wire SATU siklus saja (untuk tes; server harus MATI saat ini)
go run ./cmd/wire

# Wire loop mandiri (HANYA bila server tidak jalan)
go run ./cmd/wire -loop
```

> **Jangan jalankan `cmd/wire -loop` bersamaan dengan server.**
> Server sudah menjalankan wire loop yang sama di dalam prosesnya.
> Dua loop akan memeriksa order yang sama — pengaman ganda di
> mitra-tools (penolakan reuse `transaction_id`) mencegah kredit
> dobel, tapi log jadi berisik dan siklusnya ganda. Berhenti bersih:
> `Ctrl+C`.

## Konfigurasi (.env) — daftar lengkap

Salin `.env.example` ke `.env`. Nilai yang tidak diisi memakai
default di bawah. **Jangan pernah** menaruh token, HP, OTP, atau
QRIS asli di `.env.example`, kode, atau README.

| Variabel | Default | Keterangan |
|---|---|---|
| `PORT` | `3101` | Port HTTP server. Bind selalu `127.0.0.1` (lokal saja). |
| `SOURCE` | `mock` | `mock` = data dummy di `data/`; `gobiz` = GoBiz asli. Tanpa fallback: sesi hilang/tidak valid → HTTP 500. |
| `POLL_ENABLED` | `false` | `true` = loop proposal server aktif. Bukan wire loop — wire loop diatur token bridge (di bawah). |
| `POLL_INTERVAL_MS` | `60000` | Interval loop proposal server (ms). |
| `DATA_DIR` | `data` | Folder `orders.dummy.json` + `mutations.dummy.json` (mode mock). |
| `GOBIZ_BASE_URL` | host dashboard GoBiz | Base URL API transaksi GoBiz. |
| `GOBIZ_AUTH_BASE_URL` | host auth GoBiz | Base URL auth (OTP/verify/config/refresh). |
| `GOBIZ_MERCHANT_ID` | _(kosong)_ | Fallback bila config login gagal mengambil merchant ID dari sesi. |
| `GOBIZ_SESSION_FILE` | `.gobiz-session.json` | File sesi (hanya accessToken/refreshToken/expiresAt + merchant bila ada). **Rahasia.** |
| `GOBIZ_TRANSACTIONS_PATH` | default kode | Path endpoint transaksi (lanjutan). |
| `GOBIZ_OTP_PATH` | default kode | Path endpoint OTP (lanjutan). |
| `GOBIZ_VERIFY_PATH` | default kode | Path endpoint verifikasi OTP (lanjutan). |
| `GOBIZ_REFRESH_PATH` | default kode | Path endpoint refresh token (lanjutan). |
| `GOBIZ_MERCHANT_CONFIG_PATH` | default kode | Path endpoint config merchant (lanjutan). |
| `POLLER_INTERNAL_TOKEN` | _(kosong)_ | Token untuk `POST /mdr/reserve`. Kosong → endpoint mati (500); salah → 401. |
| `POLLER_MDR_DURATION_MS` | `900000` (= 15 mnt) | Durasi reservasi MDR. Bukan kontrak topup global (60+15 mnt). |
| `DATABASE_URL` | _(kosong)_ | DSN Postgres store reservations + claims. Kosong = MemoryStore (klaim hilang saat restart). **Produksi wajib diisi.** |
| `MITRA_BASE_URL` | `http://localhost:8080` | Alamat backend mitra-tools. |
| `BRIDGE_INTERNAL_TOKEN` | _(kosong)_ | **Wajib** untuk wire. Kosong → wire loop mati (server tetap jalan, settlement otomatis tidak jalan). Berbeda dari `POLLER_INTERNAL_TOKEN`. |
| `WIRE_POLL_INTERVAL_MS` | `3000` | Interval wire loop (ms). |
| `WIRE_MAX_IDLE_MS` | `300000` | Batas jeda idle/error wire loop (ms). Backoff error: 60/120/240 detik. |
| `WIRE_LOG_PATH` | `wire-decisions.log` | File log keputusan wire (satu baris JSON per siklus). |
| `UI_PATH` | `web/ui.html` | File dashboard yang dibaca server tiap request. |
| `WIRE_BINARY_PATH` | _(kosong)_ | Path binary wire eksternal (opsi lanjutan). |

## Endpoint (http://127.0.0.1:3101)

Server hanya bisa diakses dari laptop yang sama (bind localhost).
Jangan expose port ini ke jaringan tanpa auth tambahan.

| Endpoint | Keterangan |
|---|---|
| `GET /` | Dashboard (UI statis). |
| `GET /health` | Cek server hidup. |
| `GET /proposals` | Usulan settlement dari mutasi GoBiz (baca saja). |
| `GET /audit` | Jejak keputusan pencocokan. |
| `GET /mutations` | Riwayat pembayaran (baca saja, terbaru dulu; query `start_time`/`end_time` RFC3339 opsional; tanpa rentang di mode gobiz = 24 jam terakhir). |
| `GET /ui/config` | Konfigurasi aman saja (tanpa token/path/merchant/HP/OTP/QRIS). |
| `GET /notified-orders` | Daftar order yang masuk lewat push dari backend. |
| `POST /ui/reserve` | Alat tes lokal: reservasi MDR (tanpa auth). |
| `POST /mdr/reserve` | Reservasi MDR, wajib `Authorization: Bearer <POLLER_INTERNAL_TOKEN>`. |
| `POST /notify-order` | Push order baru dari backend — hanya membangunkan wire loop (tanpa auth, lokal saja). |

## Aturan pencocokan (yang wajib dipahami pemakai)

- Nominal harus **persis sama**: `total = harga + MDR + fee`.
  MDR = kode unik (199–1999). Fee = `ceil(harga × 0,3%)` hanya bila
  harga > Rp 500.000, bila tidak fee = 0.
- Mutasi GoBiz datang dalam **satuan minor (rupiah × 100)** — misal
  Rp 2.780 tampil sebagai `278000`. Robot membagi 100; mutasi yang
  tidak habis dibagi 100 dilewati.
- Jendela cocok: `created_at <= transacted_at <= grace_end_at`
  (60 menit + 15 menit grace = 75 menit).
- Satu `transaction_id` hanya untuk satu order (yang terawal menang).
- Bila order mengirim rincian price/fee/mdr, MDR tersirat dari mutasi
  (`amount - price - fee`) **wajib sama** dengan MDR order — kalau
  beda, tidak diusulkan (alasan `mdr-tidak-cocok`). MDR **tidak
  pernah dikarang**.
- Semua keputusan tercatat di `wire-decisions.log` (JSON per baris:
  `transaction_id`, `amount`, `transacted_at`, `order_id`,
  `decision`, `reason`, `mdr_order`, `mdr_tersirat`, `mdr_cocok`).

## Troubleshooting

| Gejala | Kemungkinan penyebab & perbaikan |
|---|---|
| Server jalan tapi `Wire loop MATI` di log | `BRIDGE_INTERNAL_TOKEN` kosong di `.env`. Isi, lalu restart server. |
| `/proposals` 500 `sumber-gobiz-error` | Sesi GoBiz hilang/kedaluwarsa. Jalankan `go run ./cmd/login` lagi. |
| `/proposals` 500 `konfigurasi-tidak-valid` | `SOURCE` bukan `mock`/`gobiz`, atau token MDR belum di-set untuk `/mdr/reserve`. |
| Order sudah dibayar tapi tetap PENDING | Cek `wire-decisions.log`: nominal tidak persis, di luar jendela 75 menit, atau MDR tidak cocok. Cek juga `/mutations` — pastikan mutasinya ada dan waktunya cocok. |
| `backend-tidak-terjangkau` di log wire | Backend mitra-tools tidak jalan atau `MITRA_BASE_URL` salah. |
| Klaim/reservasi hilang setelah restart | `DATABASE_URL` kosong → MemoryStore. Isi `DATABASE_URL` Postgres, buat databasenya dulu, lalu restart. |
| Port 3101 sudah dipakai | Server lain masih jalan di port itu, atau `PORT` di `.env` bentrok. Matikan proses lama dulu. |
| `go build` gagal `requires go >= 1.26` | Update Go di laptop ke 1.26 atau lebih baru. |

## File penting (jangan dihapus/dibagikan sembarangan)

- `.env` — berisi token. **Tidak di-commit** (sudah di `.gitignore`).
- `.gobiz-session.json` — sesi login GoBiz (token plaintext).
  **Tidak di-commit.** Hapus file ini = logout; login ulang untuk
  sesi baru.
- `wire-decisions.log` — jejak settlement, aman dibaca, tapi tidak
  di-commit.
- `data/*.json` — data dummy mode mock. Boleh dipakai untuk tes,
  jangan dipakai untuk settlement sungguhan.

## Struktur file

```
go.mod, go.sum, .env.example, .gitignore
cmd/server/main.go   # server poller + wire loop internal (1 proses)
cmd/login/main.go    # login GoBiz interaktif (HP + OTP)
cmd/wire/main.go     # wire: satu siklus, atau -loop (mandiri; JANGAN
                     #   jalan bersamaan dengan server)
internal/config/     # konfigurasi server dari env
internal/types/      # OrderPending, Mutasi, Proposal, AuditLogEntry
internal/match/      # pencocokan murni + cek identitas MDR + backoff
internal/mdr/        # fee, breakdown, kode unik crypto/rand
internal/gobiz/      # klien GoBiz + sesi + pagination
internal/mock/       # loader file dummy data/
internal/store/      # Store: MemoryStore + PostgresStore (GORM)
internal/reserve/    # reserveTotal (idempotent, anti-tabrakan)
internal/bridge/     # jembatan POC (mock, bukan endpoint asli)
internal/wire/       # wiring ke mitra-tools + jeda loop
internal/wirelog/    # log keputusan wire ke file
internal/server/     # aplikasi Fiber + handler + siklus poller
web/ui.html          # dashboard poller
data/                # orders.dummy.json + mutations.dummy.json (dummy)
```

## Riwayat & catatan kejujuran

- Migrasi dari proyek TypeScript/Bun lama ke Go pada
  2026-10-29. Versi TS lama tetap ada berdampingan dan tidak
  dihapus.
- Verifikasi sandbox 2026-10-29: `gofmt` bersih, `go vet` exit 0,
  `go build` exit 0, `go test ./...` **39 pass, 0 fail**.
- Server + wire menjadi **satu proses** sejak 2026-10-29 (satu
  perintah `go run ./cmd/server`). Skrip `start-gopay.ps1` /
  `stop-gopay.ps1` lama sudah tidak berlaku.
- Settlement live sudah terbukti jalan (dua order topup lunas
  lewat wire, MDR cocok) — tapi **store Postgres belum pernah
  diuji live di sandbox** (tidak ada Postgres di sana). Sebelum
  memakai untuk apa pun selain demo, uji `DATABASE_URL` di laptop
  dan pastikan tabel `reservations` + `claims` terbentuk
  (AutoMigrate).
- Jangan arahkan `DATABASE_URL` ke database mitra-tools — store
  poller harus database/skema sendiri.
