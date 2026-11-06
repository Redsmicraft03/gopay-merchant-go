// Package wire adalah wiring GoPay -> backend: dari usulan
// (proposal) menjadi settlement nyata, SATU SIKLUS per pemanggilan
// (bukan loop; loop ada di cmd/wire dengan flag -loop).
//
// JANGAN TERTUKAR dengan jembatan POC di internal/bridge:
//   - bridge memakai path /api/internal/pending-orders dan
//     /api/internal/proposals, dan usulan di sana BUKAN kredit.
//   - File ini memakai path BARU yang berbeda:
//     GET  {base}/api/internal/topup/pending-orders
//     POST {base}/api/internal/topup/settlements
//     supaya settlement tidak tertukar dengan proposal non-kredit.
//
// Wiring ini TIDAK memakai reserveTotal untuk order backend.
// Total backend datang apa adanya dari endpoint pending-orders
// sebagai total_snapshot — sejak 2026-10-28 backend menyimpan MDR
// (kode unik, 199-1999) per order, sehingga total_snapshot = harga
// + MDR + fee (fee 0,3% x harga hanya bila harga > 500.000).
// Wire TIDAK PERNAH menebak atau mengarang kode unik: ia hanya
// membandingkan nominal transaksi dengan total_snapshot itu persis,
// PLUS cek identitas MDR eksplisit bila backend mengirim price/fee/mdr
// (lihat internal/match).
//
// KETERBATASAN YANG DICATAT, BUKAN DISEMBUNYIKAN: bila dua order
// PENDING punya total_snapshot yang sama persis, match memilih order
// dengan created_at terawal, dan backend menolak reuse
// transaction_id (satu transaksi hanya untuk satu order).
//
// Keamanan: file ini TIDAK membaca file sesi / .env apa pun secara
// langsung. Konfigurasi GoBiz berasal dari gobiz.Config yang dipakai
// pemanggil CLI; token backend hanya diteruskan sebagai header
// Authorization dan TIDAK PERNAH dicetak ke log / output oleh
// fungsi-fungsi di sini.
//
// Wire TIDAK PERNAH mengakses DB backend langsung dan TIDAK
// PERNAH menambah saldo sendiri. Yang menambah saldo tetap backend
// lewat endpoint settlement-nya, dengan penolakan reuse
// transaction_id sebagai pengaman anti-dobel-kredit.
package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gopay-merchant-go/internal/gobiz"
	"gopay-merchant-go/internal/match"
	"gopay-merchant-go/internal/types"
)

// SettleInput adalah body POST settlement ke backend.
type SettleInput struct {
	OrderID       string `json:"order_id"`
	TransactionID string `json:"transaction_id"`
	Amount        int64  `json:"amount"`
	TransactedAt  string `json:"transacted_at"`
}

// SettleResult adalah hasil POST settlement. Ok berarti HTTP 200
// persis. Status 4xx TIDAK dilempar: pemanggil yang memutuskan
// (masuk Rejected, transaksi TIDAK diklaim). Error jaringan tetap
// dilempar oleh fetch (bukan 4xx).
type SettleResult struct {
	Ok     bool
	Status int
	Body   any
}

// RejectedSettlement adalah satu proposal yang ditolak backend.
type RejectedSettlement struct {
	Proposal types.Proposal
	Status   int
	Body     any
}

// CycleResult adalah hasil satu siklus wiring.
type CycleResult struct {
	Settled        []types.Proposal
	Rejected       []RejectedSettlement
	Claimed        map[string]bool // Set klaim BARU (input tidak diubah).
	SkippedPolling bool
	Decisions      []types.AuditLogEntry
}

func joinBase(base string) string { return strings.TrimRight(base, "/") }

func parseOrderPending(m map[string]any) (types.OrderPending, error) {
	orderID, _ := m["order_id"].(string)
	if orderID == "" {
		return types.OrderPending{}, fmt.Errorf("pending-orders: order_id kosong / bukan string")
	}
	total, ok := parseIntField(m["total_snapshot"])
	if !ok || total < 0 {
		return types.OrderPending{}, fmt.Errorf("pending-orders: total_snapshot bukan integer >= 0")
	}
	created, _ := m["created_at"].(string)
	expiry, _ := m["expiry_at"].(string)
	grace, _ := m["grace_end_at"].(string)
	for field, v := range map[string]string{"created_at": created, "expiry_at": expiry, "grace_end_at": grace} {
		if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
			if _, err2 := time.Parse(time.RFC3339, v); err2 != nil {
				return types.OrderPending{}, fmt.Errorf("pending-orders: %s tidak parseable", field)
			}
		}
	}
	status, _ := m["status"].(string)
	o := types.OrderPending{
		OrderID: orderID, TotalSnapshot: total, CreatedAt: created,
		ExpiryAt: expiry, GraceEndAt: grace, Status: status,
	}
	// Rincian MDR opsional: bila dikirim tapi bukan integer/null,
	// tolak responsnya (bukan isi tebakan).
	for _, field := range []string{"price", "fee", "mdr"} {
		v, present := m[field]
		if !present || v == nil {
			continue
		}
		n, ok := parseIntField(v)
		if !ok || n < 0 {
			return types.OrderPending{}, fmt.Errorf("pending-orders: %s bukan integer/null", field)
		}
		switch field {
		case "price":
			o.Price = types.IntPtr(n)
		case "fee":
			o.Fee = types.IntPtr(n)
		case "mdr":
			o.MDR = types.IntPtr(n)
		}
	}
	return o, nil
}

func parseIntField(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

// FetchBackendPendingOrders melakukan
// GET {base}/api/internal/topup/pending-orders dengan header
// Authorization: Bearer <token>. Melempar error yang jelas bila HTTP
// tidak OK atau bentuk respons tidak valid. Token TIDAK PERNAH
// dimasukkan ke pesan error.
func FetchBackendPendingOrders(ctx context.Context, client *http.Client, baseURL, token string) ([]types.OrderPending, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinBase(baseURL)+"/api/internal/topup/pending-orders", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("fetchBackendPendingOrders: backend menjawab HTTP %d untuk GET /api/internal/topup/pending-orders", res.StatusCode)
	}
	dec := json.NewDecoder(res.Body)
	dec.UseNumber()
	var raw []map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("fetchBackendPendingOrders: respons backend bukan JSON array yang valid")
	}
	orders := make([]types.OrderPending, 0, len(raw))
	for i, item := range raw {
		o, err := parseOrderPending(item)
		if err != nil {
			return nil, fmt.Errorf("fetchBackendPendingOrders: pending-orders[%d]: %w", i, err)
		}
		orders = append(orders, o)
	}
	return orders, nil
}

// SettleBackendOrder melakukan POST {base}/api/internal/topup/settlements.
// TIDAK melempar untuk 4xx/5xx yang masih berupa Response: pemanggil
// yang memutuskan dari {Ok, Status, Body}.
func SettleBackendOrder(ctx context.Context, client *http.Client, baseURL, token string, input SettleInput) (SettleResult, error) {
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(input)
	if err != nil {
		return SettleResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinBase(baseURL)+"/api/internal/topup/settlements", bytes.NewReader(body))
	if err != nil {
		return SettleResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return SettleResult{}, err
	}
	defer res.Body.Close()
	var parsed any
	dec := json.NewDecoder(res.Body)
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		parsed = nil
	}
	return SettleResult{Ok: res.StatusCode == 200, Status: res.StatusCode, Body: parsed}, nil
}

// CycleInput adalah input satu siklus wiring atas data yang sudah
// diambil pemanggil.
type CycleInput struct {
	Orders    []types.OrderPending
	Mutations []types.Mutasi
	Claimed   map[string]bool
	Settle    func(ctx context.Context, proposal types.Proposal) (SettleResult, error)
	Options   match.MatchOptions
}

// RunWireCycle menjalankan satu siklus wiring.
//
//   - match yang menentukan proposal, dalam urutannya.
//   - Setiap proposal di-settle SATU PER SATU, sesuai urutan proposal.
//   - Bila settle ok (HTTP 200): proposal masuk Settled dan
//     transaction_id-nya diklaim.
//   - Bila settle tidak ok: proposal masuk Rejected dan transaction_id
//     TIDAK diklaim, supaya bisa dicoba manual / pada siklus berikutnya.
//   - Bila tidak ada order PENDING: SkippedPolling true dan Settle
//     TIDAK pernah dipanggil.
//
// PERHATIAN soal klaim: klaim hasil akhir hanya mengklaim transaksi
// yang benar-benar settled. Klaim input pemanggil tidak pernah diubah
// (map baru).
func RunWireCycle(ctx context.Context, input CycleInput) (CycleResult, error) {
	matched := match.MatchOrders(input.Orders, input.Mutations, input.Claimed, input.Options)
	if matched.SkippedPolling {
		return CycleResult{
			Settled: nil, Rejected: nil, Claimed: matched.Claimed,
			SkippedPolling: true, Decisions: matched.Decisions,
		}, nil
	}
	claimed := make(map[string]bool, len(matched.Claimed))
	for k, v := range matched.Claimed {
		claimed[k] = v
	}
	// Klaim hasil akhir hanya untuk yang settled: hapus klaim proposal
	// yang belum di-settle dari salinan, lalu tambah satu per satu.
	for k := range claimed {
		if !input.Claimed[k] {
			delete(claimed, k)
		}
	}
	var settled []types.Proposal
	var rejected []RejectedSettlement
	for _, proposal := range matched.Proposals {
		result, err := input.Settle(ctx, proposal)
		if err != nil {
			return CycleResult{}, err
		}
		if result.Ok {
			settled = append(settled, proposal)
			claimed[proposal.TransactionID] = true
		} else {
			rejected = append(rejected, RejectedSettlement{Proposal: proposal, Status: result.Status, Body: result.Body})
		}
	}
	return CycleResult{
		Settled: settled, Rejected: rejected, Claimed: claimed,
		SkippedPolling: false, Decisions: matched.Decisions,
	}, nil
}

// LiveCycleInput adalah input satu siklus wiring live.
type LiveCycleInput struct {
	BackendBaseURL string
	Token          string
	GobizConfig    gobiz.Config
	Claimed        map[string]bool
	// Session opsional untuk tes: bila diisi, file sesi TIDAK dibaca.
	Session *gobiz.Session
	// HTTPClient opsional untuk tes (nil = default).
	HTTPClient *http.Client
}

// RunLiveWireCycle menjalankan satu siklus wiring live: ambil pending
// orders dari backend, lalu (hanya bila ada PENDING) ambil mutasi
// GoBiz, lalu settle.
//
//   - Bila tidak ada order PENDING: kembalikan skipped TANPA memanggil
//     GoBiz sama sekali.
//   - Jendela GoBiz: startTime = created_at paling awal, endTime =
//     grace_end_at paling akhir di antara order PENDING.
//   - Error GoBiz DILEMPAR apa adanya, TANPA fallback ke mock.
//   - Fungsi ini TIDAK melakukan login GoBiz / OTP apa pun.
//   - Bila backend tidak terjangkau, siklus GAGAL JELAS dengan pesan
//     backend-tidak-terjangkau — bukan skipped diam-diam.
func RunLiveWireCycle(ctx context.Context, input LiveCycleInput) (CycleResult, error) {
	client := input.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	all, err := FetchBackendPendingOrders(ctx, client, input.BackendBaseURL, input.Token)
	if err != nil {
		return CycleResult{}, fmt.Errorf("backend-tidak-terjangkau: %w", err)
	}
	var pending []types.OrderPending
	for _, o := range all {
		if o.Status == "PENDING" {
			pending = append(pending, o)
		}
	}
	if len(pending) == 0 {
		return CycleResult{
			Settled: nil, Rejected: nil, Claimed: copyClaimed(input.Claimed),
			SkippedPolling: true, Decisions: nil,
		}, nil
	}
	var startTime, endTime string
	for i, o := range pending {
		if i == 0 || o.CreatedAt < startTime {
			startTime = o.CreatedAt
		}
		if i == 0 || o.GraceEndAt > endTime {
			endTime = o.GraceEndAt
		}
	}
	// Perbandingan string ISO 8601 dengan zona yang sama bekerja untuk
	// data normal; untuk keamanan, window dihitung ulang dari waktu
	// yang diparse bila formatnya RFC3339.
	if s, e, ok := windowFromParsed(pending); ok {
		startTime, endTime = s, e
	}
	mutations, _, err := input.GobizConfig.FetchTransactionsDetailed(ctx, gobiz.FetchOptions{
		StartTime: startTime, EndTime: endTime, Session: input.Session,
	})
	if err != nil {
		return CycleResult{}, err
	}
	return RunWireCycle(ctx, CycleInput{
		Orders: pending, Mutations: mutations, Claimed: input.Claimed,
		Settle: func(ctx context.Context, proposal types.Proposal) (SettleResult, error) {
			return SettleBackendOrder(ctx, client, input.BackendBaseURL, input.Token, SettleInput{
				OrderID: proposal.OrderID, TransactionID: proposal.TransactionID,
				Amount: proposal.Amount, TransactedAt: proposal.TransactedAt,
			})
		},
	})
}

func copyClaimed(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func windowFromParsed(pending []types.OrderPending) (string, string, bool) {
	var start, end time.Time
	for i, o := range pending {
		created, err1 := time.Parse(time.RFC3339Nano, o.CreatedAt)
		grace, err2 := time.Parse(time.RFC3339Nano, o.GraceEndAt)
		if err1 != nil || err2 != nil {
			created, err1 = time.Parse(time.RFC3339, o.CreatedAt)
			grace, err2 = time.Parse(time.RFC3339, o.GraceEndAt)
			if err1 != nil || err2 != nil {
				return "", "", false
			}
		}
		if i == 0 || created.Before(start) {
			start = created
		}
		if i == 0 || grace.After(end) {
			end = grace
		}
	}
	return start.Format(time.RFC3339), end.Format(time.RFC3339), true
}

// ---------- Jeda loop wire (murni, mudah dites tanpa tidur) ----------

// DefaultWireIntervalMs adalah interval default loop wire (3 detik,
// sejak 2026-10-29: 60 detik kelamaan). Interval ini dipakai
// saat ada order PENDING (siklus penuh ke GoBiz) MAUPUN saat idle
// (hanya cek backend pending, murah, lokal, TANPA panggil GoBiz).
// Deteksi order baru jadi maksimal 3 detik, bukan 60 detik.
const DefaultWireIntervalMs = int64(3_000)

// DefaultWireMaxIdleMs adalah batas jeda idle/error loop wire (300 detik).
const DefaultWireMaxIdleMs = int64(300_000)

// WireErrorBackoffMs adalah urutan backoff error: 60s, 120s, 240s.
var WireErrorBackoffMs = []int64{60_000, 120_000, 240_000}

// WireIdleDelayMs TIDAK DIPAKAI LAGI sejak interval idle = interval
// aktif (3 detik, hanya cek backend). Dibiarkan supaya kode lama yang
// merujuknya tidak rusak, tapi NextLoopDelayMs tidak lagi
// menggunakannya untuk skipped.
var WireIdleDelayMs = []int64{120_000, 240_000}

// ParseIntervalMs mem-parse interval dari env: hanya integer > 0
// yang diterima, sisanya jatuh ke default. Tidak pernah 0/negatif.
func ParseIntervalMs(raw string, fallback int64) int64 {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}

// LoopDelayInput adalah input perhitungan jeda loop berikutnya.
type LoopDelayInput struct {
	HadError            bool
	Skipped             bool
	ConsecutiveFailures int64
	ConsecutiveSkipped  int64
	BaseMs              int64
	MaxIdleMs           int64
}

// NextLoopDelayMs menghitung jeda berikutnya, murni dari hitungan:
//   - error -> backoff error (60/120/240 detik, lalu maxIdleMs);
//   - skipped -> baseMs (cek backend tiap baseMs, tanpa GoBiz);
//   - sukses biasa -> baseMs.
//
// maxIdleMs selalu menjadi batas atas.
func NextLoopDelayMs(input LoopDelayInput) int64 {
	capMs := input.MaxIdleMs
	if capMs < 1 {
		capMs = 1
	}
	if input.HadError {
		idx := input.ConsecutiveFailures - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= int64(len(WireErrorBackoffMs)) {
			idx = int64(len(WireErrorBackoffMs)) - 1
		}
		delay := WireErrorBackoffMs[idx]
		if input.ConsecutiveFailures > int64(len(WireErrorBackoffMs)) {
			delay = capMs
		}
		if delay > capMs {
			delay = capMs
		}
		return delay
	}
	// Skipped = tidak ada order PENDING: siklus itu HANYA cek backend
	// (tanpa GoBiz, tanpa tulis log, lihat cmd/wire). Jeda idle TIDAK
	// lagi memanjang, supaya order baru terdeteksi maksimal BaseMs
	// (default 3 detik) setelah dibuat, bukan 120-300 detik kemudian.
	// Cek backend lokal tiap 3 detik itu murah; yang berhenti saat
	// tidak ada order adalah pengecekan ke GoBiz, bukan deteksinya.
	if input.Skipped {
		if input.BaseMs > capMs {
			return capMs
		}
		return input.BaseMs
	}
	if input.BaseMs > capMs {
		return capMs
	}
	return input.BaseMs
}
