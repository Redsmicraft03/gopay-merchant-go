// Command wire adalah CLI wiring GoPay -> mitra-tools.
//
// Pemakaian (dari folder proyek):
//
//	go run ./cmd/wire            # satu siklus (bukan loop)
//	go run ./cmd/wire -loop      # loop berkala, berhenti Ctrl+C
//
// Ini CLI TERPISAH dari server poller. Server /proposals TIDAK
// diubah perilakunya oleh file ini: wire hanya memanggil
// RunLiveWireCycle berulang / sekali, memakai endpoint internal
// mitra-tools yang SUDAH ADA (GET /api/internal/topup/pending-orders
// dan POST /api/internal/topup/settlements), dan TIDAK mengubah
// logika settlement di mitra-tools.
//
// CATATAN KEJUJURAN: asumsi settlement otomatis ini — paymentReference
// = transaction_id GoBiz — MASIH MENUNGGU KUNCI BAGUS. Total
// mitra-tools kini sudah termasuk MDR (kode unik yang disimpan backend
// per order): total = harga + MDR + fee. Wire ini untuk TES LOKAL
// dulu, BUKAN produksi.
//
// Environment — SAMA dengan wire-cli TS, tidak ada variabel baru
// kecuali flag -loop:
//   - MITRA_BASE_URL (opsional, default http://localhost:8080).
//   - BRIDGE_INTERNAL_TOKEN (wajib): token internal mitra-tools untuk
//     endpoint /api/internal/topup/*. Bila kosong, CLI keluar dengan
//     exit 1 TANPA panggilan apa pun.
//   - WIRE_POLL_INTERVAL_MS (opsional, default 3000): jeda antar
//     siklus. 3 detik saat ada order PENDING (siklus penuh ke GoBiz)
//     maupun saat idle (hanya cek backend pending, tanpa GoBiz).
//     60 detik sudah tidak dipakai lagi (perintah Hanif 2026-10-29:
//     kelamaan).
//   - WIRE_MAX_IDLE_MS (opsional, default 300000): batas jeda saat
//     tidak ada order PENDING (skipped) maupun saat error beruntun.
//   - Variabel GoBiz (GOBIZ_*) dibaca oleh gobiz.ConfigFromEnv();
//     wire ini TIDAK melakukan login GoBiz / OTP apa pun. Sesi GoBiz
//     harus sudah ada (lihat cmd/login) sebelum wire jalan.
//
// Perilaku loop:
//   - Klaim antar siklus disimpan di memori proses yang sama, jadi
//     satu transaksi tidak di-settle ulang selama proses ini hidup.
//     Klaim TIDAK persisten antar proses: bila proses di-restart,
//     klaim kembali kosong — pengaman gandanya adalah mitra-tools
//     sendiri yang menolak reuse transaction_id (409).
//   - Bila tidak ada order PENDING: siklus skipped, GoBiz tidak
//     dipanggil, siklus itu TIDAK ditulis ke wire-decisions.log,
//     dan jeda berikutnya tetap interval aktif (default 3 detik)
//     hanya untuk cek backend, supaya order baru terdeteksi dalam
//     3 detik. Yang berhenti saat tidak ada order adalah pengecekan
//     ke GoBiz, bukan deteksi ordernya (deteksi nol sama sekali
//     hanya mungkin lewat dorongan dari backend, bukan lewat
//     polling; itu perubahan arsitektur, bukan loop).
//   - Bila satu siklus error: error dicatat, loop TIDAK crash, jeda
//     berikutnya memakai backoff error sampai WIRE_MAX_IDLE_MS.
//   - Setiap siklus mencetak SATU ringkasan JSON ke stdout DAN
//     menulis SATU baris JSON yang sama + keputusan matching ke FILE
//     wire-decisions.log (atau WIRE_LOG_PATH) — lihat internal/wirelog.
//     TANPA token, TANPA detail sesi GoBiz, TANPA nilai environment.
//   - Ctrl+C (SIGINT) / SIGTERM menghentikan loop dengan bersih:
//     tidak ada siklus baru yang dimulai, lalu proses keluar exit 0.
//
// Output satu siklus: ringkasan JSON ke stdout — TANPA token, TANPA
// detail sesi GoBiz, TANPA nilai environment apa pun. Exit 0 bila
// siklus selesai (walau ada rejected); exit 1 bila error konfigurasi,
// error GoBiz, atau mitra-tools tidak terjangkau.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"mitrain-gopay/internal/gobiz"
	"mitrain-gopay/internal/wire"
	"mitrain-gopay/internal/wirelog"
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func summaryJSON(result wire.CycleResult) map[string]any {
	settledIDs := make([]string, 0, len(result.Settled))
	for _, p := range result.Settled {
		settledIDs = append(settledIDs, p.OrderID)
	}
	rejected := make([]map[string]any, 0, len(result.Rejected))
	for _, r := range result.Rejected {
		rejected = append(rejected, map[string]any{
			"order_id":       r.Proposal.OrderID,
			"transaction_id": r.Proposal.TransactionID,
			"status":         r.Status,
			"body":           r.Body,
		})
	}
	return map[string]any{
		"settled_count":  len(result.Settled),
		"rejected_count": len(result.Rejected),
		"skipped":        result.SkippedPolling,
		"settled_ids":    settledIDs,
		"rejected":       rejected,
	}
}

func logEntry(result *wire.CycleResult, cycle *int, hadError bool, errMsg string) wirelog.Entry {
	entry := wirelog.Entry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Cycle:     cycle,
		Skipped:   false, HadError: hadError,
		SettledIDs: []string{}, Rejected: []wirelog.RejectedEntry{},
		Decisions: []wirelog.DecisionEntry{},
	}
	if errMsg != "" {
		entry.Error = errMsg
	}
	if result != nil {
		entry.SettledCount = len(result.Settled)
		entry.RejectedCount = len(result.Rejected)
		entry.Skipped = result.SkippedPolling
		for _, p := range result.Settled {
			entry.SettledIDs = append(entry.SettledIDs, p.OrderID)
		}
		for _, r := range result.Rejected {
			entry.Rejected = append(entry.Rejected, wirelog.RejectedEntry{
				OrderID: r.Proposal.OrderID, TransactionID: r.Proposal.TransactionID, Status: r.Status,
			})
		}
		entry.Decisions = wirelog.DecisionLogEntries(result.Decisions)
	}
	return entry
}

func printJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func runOnce(ctx context.Context, baseURL, token string, cfg gobiz.Config, claimed map[string]bool) (wire.CycleResult, map[string]bool, error) {
	result, err := wire.RunLiveWireCycle(ctx, wire.LiveCycleInput{
		MitraBaseURL: baseURL, Token: token, GobizConfig: cfg, Claimed: claimed,
	})
	if err != nil {
		return wire.CycleResult{}, claimed, err
	}
	return result, result.Claimed, nil
}

func main() {
	// .env di folder kerja dibaca bila ada; env manual tetap menang.
	_ = godotenv.Load()
	loop := flag.Bool("loop", false, "Jalankan loop berkala (tanpa flag = satu siklus).")
	flag.Parse()

	baseURL := envOr("MITRA_BASE_URL", "http://localhost:8080")
	token := os.Getenv("BRIDGE_INTERNAL_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "wire: konfigurasi tidak lengkap: BRIDGE_INTERNAL_TOKEN kosong. Isi token internal mitra-tools (berbeda dari POLLER_INTERNAL_TOKEN) sebelum menjalankan wire; tidak ada panggilan yang dilakukan.")
		os.Exit(1)
	}
	cfg := gobiz.ConfigFromEnv()
	ctx := context.Background()

	if !*loop {
		result, _, err := runOnce(ctx, baseURL, token, cfg, make(map[string]bool))
		if err != nil {
			fmt.Fprintf(os.Stderr, "wire: siklus gagal: %s\n", err.Error())
			wirelog.AppendWireLog(logEntry(nil, nil, true, err.Error()))
			os.Exit(1)
		}
		printJSON(summaryJSON(result))
		wirelog.AppendWireLog(logEntry(&result, nil, false, ""))
		return
	}

	// Mode loop — logikanya sekarang di wire.RunLoop (dipakai bersama
	// cmd/server, supaya server + wire jadi 1 proses). Perilaku sama
	// persis dengan loop lama di sini.
	baseMs := wire.ParseIntervalMs(os.Getenv("WIRE_POLL_INTERVAL_MS"), wire.DefaultWireIntervalMs)
	maxIdleMs := wire.ParseIntervalMs(os.Getenv("WIRE_MAX_IDLE_MS"), wire.DefaultWireMaxIdleMs)
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	wire.RunLoop(ctx, wire.LoopInput{
		MitraBaseURL: baseURL, Token: token, GobizConfig: cfg,
		BaseMs: baseMs, MaxIdleMs: maxIdleMs,
	})
}
