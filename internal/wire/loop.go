// Loop wire settlement berkala, dipakai BERSAMA oleh dua command:
//   - cmd/wire -loop  (proses wire mandiri, cara lama — tetap jalan)
//   - cmd/server      (server GoPay menjalankan loop ini DI DALAM
//     prosesnya sendiri sejak 2026-10-29, keputusan: server +
//     wire jadi 1 proses, 1 perintah, 1 terminal)
//
// Perilakunya SAMA PERSIS dengan loop lama di cmd/wire/main.go
// (kode itu dipindahkan ke sini tanpa mengubah logika, supaya tidak
// ada dua versi loop yang bisa menyimpang):
//   - tiap siklus: RunLiveWireCycle (cek backend pending, lalu —
//     hanya bila ada PENDING — cek GoBiz, lalu settle)
//   - bila tidak ada PENDING: skipped, GoBiz TIDAK dipanggil,
//     siklus TIDAK ditulis ke wire-decisions.log dan TIDAK dicetak,
//     jeda berikutnya tetap BaseMs (default 3 detik, hanya cek backend)
//   - bila error: dicatat, loop TIDAK crash, jeda memakai backoff
//     error (60/120/240 detik, dibatasi MaxIdleMs)
//   - klaim antar siklus disimpan di memori proses yang sama;
//     pengaman ganda tetap backend yang menolak reuse
//     transaction_id (409)
//   - Wake (bila tidak nil) membangunkan loop dari tidurnya:
//     dipakai server saat menerima push /notify-order, supaya siklus
//     berikutnya jalan segera, bukan menunggu sisa jeda 3 detik.
//     Wake berbuffer 1 di pemanggil; pengiriman yang gagal karena
//     buffer penuh diabaikan (loop sudah bangun / sedang jalan).
package wire

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gopay-merchant-go/internal/gobiz"
	"gopay-merchant-go/internal/wirelog"
)

// LoopInput adalah konfigurasi loop wire. Semua field dibaca dari
// env yang sama dengan cmd/wire oleh pemanggil (bukan oleh RunLoop).
type LoopInput struct {
	BackendBaseURL string
	Token          string
	GobizConfig    gobiz.Config
	BaseMs         int64
	MaxIdleMs      int64
	// Wake, bila tidak nil, membangunkan loop dari jeda antar siklus.
	Wake <-chan struct{}
}

func loopLogEntry(result *CycleResult, cycle *int, hadError bool, errMsg string) wirelog.Entry {
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

func printLoopJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

// RunLoop menjalankan loop wire sampai ctx dibatalkan, lalu kembali.
// Mencetak wire-loop-started di awal dan wire-loop-stopped di akhir
// (format sama dengan loop lama), dan TIDAK PERNAH mengembalikan
// error untuk kegagalan siklus — kegagalan siklus hanya dicatat dan
// di-backoff, loop terus jalan sampai ctx dibatalkan.
func RunLoop(ctx context.Context, input LoopInput) {
	baseMs := input.BaseMs
	if baseMs < 1 {
		baseMs = DefaultWireIntervalMs
	}
	maxIdleMs := input.MaxIdleMs
	if maxIdleMs < 1 {
		maxIdleMs = DefaultWireMaxIdleMs
	}
	claimed := make(map[string]bool)
	cycle := 0
	var consecutiveFailures, consecutiveSkipped int64
	printLoopJSON(map[string]any{
		"event": "wire-loop-started", "baseMs": baseMs, "maxIdleMs": maxIdleMs, "backendBaseUrl": input.BackendBaseURL,
	})
	for {
		select {
		case <-ctx.Done():
			printLoopJSON(map[string]any{"event": "wire-loop-stopped", "cycles": cycle})
			return
		default:
		}
		cycle++
		result, err := RunLiveWireCycle(ctx, LiveCycleInput{
			BackendBaseURL: input.BackendBaseURL, Token: input.Token,
			GobizConfig: input.GobizConfig, Claimed: claimed,
		})
		var entry wirelog.Entry
		var delayInput LoopDelayInput
		if err != nil {
			consecutiveFailures++
			consecutiveSkipped = 0
			entry = loopLogEntry(nil, &cycle, true, err.Error())
			delayInput = LoopDelayInput{HadError: true, ConsecutiveFailures: consecutiveFailures, BaseMs: baseMs, MaxIdleMs: maxIdleMs}
		} else {
			claimed = result.Claimed
			if result.SkippedPolling {
				consecutiveFailures = 0
				consecutiveSkipped++
				delayInput = LoopDelayInput{Skipped: true, ConsecutiveSkipped: consecutiveSkipped, BaseMs: baseMs, MaxIdleMs: maxIdleMs}
			} else {
				consecutiveFailures = 0
				consecutiveSkipped = 0
				delayInput = LoopDelayInput{BaseMs: baseMs, MaxIdleMs: maxIdleMs}
			}
			entry = loopLogEntry(&result, &cycle, false, "")
		}
		// Siklus skipped tidak ditulis ke log dan tidak dicetak.
		if !result.SkippedPolling || err != nil {
			printLoopJSON(entry)
			wirelog.AppendWireLog(entry)
		}
		delay := NextLoopDelayMs(delayInput)
		select {
		case <-ctx.Done():
			printLoopJSON(map[string]any{"event": "wire-loop-stopped", "cycles": cycle})
			return
		case <-time.After(time.Duration(delay) * time.Millisecond):
		case <-input.Wake:
		}
	}
}
