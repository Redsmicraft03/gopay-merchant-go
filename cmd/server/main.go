// Command server menjalankan HTTP server poller GoBiz (Fiber v3).
//
// Pemakaian (dari folder proyek):
//
//	go run ./cmd/server
//	# atau, setelah build:
//	./gopay-merchant-go-server
//
// Konfigurasi lewat env (lihat .env.example; file .env di folder
// kerja dibaca otomatis via godotenv, env manual tetap menang): PORT (default 3101,
// bind selalu 127.0.0.1), POLL_INTERVAL_MS, DATA_DIR, POLL_ENABLED
// (default mati), SOURCE (mock/gobiz), GOBIZ_*, POLLER_INTERNAL_TOKEN,
// POLLER_MDR_DURATION_MS, DATABASE_URL (Postgres; kosong = MemoryStore
// demo, klaim hilang saat restart — produksi WAJIB DATABASE_URL),
// UI_PATH (default web/ui.html).
//
// Server + wire jadi SATU PROSES sejak 2026-10-29 (keputusan:
// nyalakan aplikasi GoPay = 1 perintah, 1 terminal, bukan server
// dan wire terpisah). Menyalakan server ini otomatis menjalankan
// wire loop settlement di dalam proses yang sama (goroutine,
// interval dari WIRE_POLL_INTERVAL_MS, default 3 detik) — selama
// BRIDGE_INTERNAL_TOKEN terisi. Bila token itu kosong, server tetap
// jalan (dashboard + /notify-order), tapi wire loop internal mati
// dan log akan mengatakannya dengan jelas; isi tokennya, lalu
// restart server. Proses wire mandiri (cmd/wire -loop) tetap bisa
// jalan sendiri, tapi JANGAN dijalankan bersamaan dengan server ini
// — dua loop akan bersaing memeriksa order yang sama (pengaman
// ganda tetap penolakan reuse transaction_id di backend, tapi
// lognya jadi berisik dan siklusnya ganda).
//
// Server ini TIDAK mengkredit sendiri: settlement tetap dilakukan
// wire loop (paket internal/wire yang sama dengan cmd/wire) lewat
// endpoint internal backend. Berhenti bersih dengan Ctrl+C
// (SIGINT) / SIGTERM: server shutdown DAN wire loop berhenti.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"

	"gopay-merchant-go/internal/config"
	"gopay-merchant-go/internal/gobiz"
	"gopay-merchant-go/internal/server"
	"gopay-merchant-go/internal/wire"
)

func main() {
	// .env di folder kerja dibaca bila ada; env yang sudah di-set
	// manual tetap menang (godotenv tidak menimpa env yang ada).
	// File .env tidak ada = jalan seperti biasa, tanpa error.
	_ = godotenv.Load()
	cfg := config.LoadServerConfig()
	srv, closeStore, err := server.NewServerFromConfig(cfg)
	if err != nil {
		log.Fatalf("server: gagal membuka store: %s", err)
	}
	defer func() {
		_ = closeStore()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.PollEnabled {
		srv.StartLoop(ctx)
		log.Printf("[poller] Loop proposal AKTIF (interval %d ms, backoff bila error).", cfg.PollIntervalMs)
	} else {
		log.Print("[poller] Loop proposal MATI (default). Set POLL_ENABLED=true untuk mengaktifkan. (Ini BUKAN wire loop — lihat baris wire loop di bawah.)")
	}

	// Wire loop internal: 1 proses dengan server. Channel bangun
	// dipakai server saat menerima push /notify-order.
	wireWake := make(chan struct{}, 1)
	if token := strings.TrimSpace(os.Getenv("BRIDGE_INTERNAL_TOKEN")); token != "" {
		srv.SetWireWake(wireWake)
		baseMs := wire.ParseIntervalMs(os.Getenv("WIRE_POLL_INTERVAL_MS"), wire.DefaultWireIntervalMs)
		maxIdleMs := wire.ParseIntervalMs(os.Getenv("WIRE_MAX_IDLE_MS"), wire.DefaultWireMaxIdleMs)
		baseURL := os.Getenv("BACKEND_BASE_URL")
		if strings.TrimSpace(baseURL) == "" {
			baseURL = "http://localhost:8080"
		}
		go wire.RunLoop(ctx, wire.LoopInput{
			BackendBaseURL: baseURL, Token: token, GobizConfig: gobiz.ConfigFromEnv(),
			BaseMs: baseMs, MaxIdleMs: maxIdleMs, Wake: wireWake,
		})
		log.Printf("[poller] Wire loop AKTIF di proses ini (interval %d ms). Server + wire = 1 proses.", baseMs)
	} else {
		log.Print("[poller] Wire loop MATI: BRIDGE_INTERNAL_TOKEN kosong. Server tetap jalan, tapi settlement otomatis tidak jalan. Isi token itu di .env, lalu restart server.")
	}

	go func() {
		<-ctx.Done()
		_ = srv.App().Shutdown()
	}()

	log.Printf("[poller] %s v%s berjalan di http://%s", server.ServiceName, server.Version, srv.ListenAddr())
	log.Print("[poller] GET / (UI) | GET /health | GET /proposals | GET /audit | GET /mutations (riwayat baca saja) | GET /ui/config | POST /ui/reserve — tidak ada endpoint ubah saldo/status.")
	if err := srv.App().Listen(srv.ListenAddr()); err != nil {
		fmt.Fprintf(os.Stderr, "server: %s\n", err)
		os.Exit(1)
	}
}
