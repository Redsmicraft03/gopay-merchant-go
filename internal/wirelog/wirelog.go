// Package wirelog menulis log keputusan matching wire ke FILE —
// bukan hanya stdout.
//
// Alasan: log stdout hilang saat terminal ditutup, sehingga kegagalan
// pencocokan ("nominal tidak cocok / di luar jendela") tampil di log
// persis seperti "tidak ada transaksi". File ini menyimpan satu baris
// JSON per siklus, berisi keputusan per transaksi yang dipertimbangkan
// (dari match), sehingga akar masalah "sudah bayar masih PENDING"
// bisa dibuktikan dari file, bukan ditebak.
//
// Isi log: waktu, settled/rejected/skipped, dan untuk tiap keputusan:
// transaction_id, amount, transacted_at, order_id, decision, reason,
// plus cek identitas MDR: mdr_order, mdr_tersirat, mdr_cocok
// (mdr_cocok nil berarti backend tidak mengirim rincian MDR,
// sehingga identitas hanya lewat total_snapshot).
// TIDAK PERNAH token, sesi GoBiz, nilai environment, nomor HP, atau
// payload QRIS. File default: wire-decisions.log di working directory
// proses (folder mitrain-gopay-go bila dijalankan dari sana); bisa
// diubah lewat env WIRE_LOG_PATH. Kegagalan menulis log TIDAK BOLEH
// menggagalkan siklus — ia dicatat ke stderr saja.
package wirelog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mitrain-gopay/internal/types"
)

// RejectedEntry adalah satu settlement yang ditolak mitra-tools.
type RejectedEntry struct {
	OrderID       string `json:"order_id"`
	TransactionID string `json:"transaction_id"`
	Status        int    `json:"status"`
}

// DecisionEntry adalah satu keputusan matching yang dicatat ke log.
type DecisionEntry struct {
	TransactionID *string        `json:"transaction_id"`
	Amount        *int64         `json:"amount"`
	TransactedAt  *string        `json:"transacted_at"`
	OrderID       *string        `json:"order_id"`
	Decision      types.Decision `json:"decision"`
	Reason        types.Reason   `json:"reason"`
	MDROrder      *int64         `json:"mdr_order"`
	MDRTersirat   *int64         `json:"mdr_tersirat"`
	MDRCocok      *bool          `json:"mdr_cocok"`
	Status        string         `json:"status,omitempty"`
	PaymentType   string         `json:"payment_type,omitempty"`
}

// Entry adalah satu baris log per siklus wire.
type Entry struct {
	Timestamp     string          `json:"timestamp"`
	Cycle         *int            `json:"cycle,omitempty"`
	SettledCount  int             `json:"settled_count"`
	SettledIDs    []string        `json:"settled_ids"`
	RejectedCount int             `json:"rejected_count"`
	Rejected      []RejectedEntry `json:"rejected"`
	Skipped       bool            `json:"skipped"`
	HadError      bool            `json:"hadError"`
	Error         string          `json:"error,omitempty"`
	Decisions     []DecisionEntry `json:"decisions"`
}

// LogPath mengembalikan path file log: env WIRE_LOG_PATH bila diisi
// (relatif diselesaikan ke working directory), bila tidak
// wire-decisions.log di working directory.
func LogPath() string {
	raw := strings.TrimSpace(os.Getenv("WIRE_LOG_PATH"))
	if raw != "" {
		if filepath.IsAbs(raw) {
			return raw
		}
		if wd, err := os.Getwd(); err == nil {
			return filepath.Join(wd, raw)
		}
		return raw
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "wire-decisions.log")
	}
	return "wire-decisions.log"
}

// AppendWireLog menulis satu baris JSON ke file log. Bila path
// diberikan, path itu dipakai (untuk tes); bila tidak, LogPath().
// Kegagalan menulis TIDAK PERNAH menggagalkan siklus — hanya stderr.
func AppendWireLog(entry Entry, path ...string) {
	file := LogPath()
	if len(path) > 0 && path[0] != "" {
		file = path[0]
	}
	if dir := filepath.Dir(file); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "wire-log: gagal menulis log keputusan (%s). Siklus tetap berjalan; periksa WIRE_LOG_PATH.\n", err.Error())
			return
		}
	}
	b, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wire-log: gagal menulis log keputusan (%s). Siklus tetap berjalan.\n", err.Error())
		return
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wire-log: gagal menulis log keputusan (%s). Siklus tetap berjalan; periksa WIRE_LOG_PATH.\n", err.Error())
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "wire-log: gagal menulis log keputusan (%s). Siklus tetap berjalan.\n", err.Error())
	}
}

// DecisionLogEntries mengubah decisions match menjadi entri log.
func DecisionLogEntries(decisions []types.AuditLogEntry) []DecisionEntry {
	out := make([]DecisionEntry, 0, len(decisions))
	for _, d := range decisions {
		out = append(out, DecisionEntry{
			TransactionID: d.TransactionID, Amount: d.Amount,
			TransactedAt: d.TransactedAt, OrderID: d.OrderID,
			Decision: d.Decision, Reason: d.Reason,
			MDROrder: d.MDROrder, MDRTersirat: d.MDRTersirat, MDRCocok: d.MDRCocok,
			Status: d.Status, PaymentType: d.PaymentType,
		})
	}
	return out
}
