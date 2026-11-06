// Package match berisi aturan pencocokan poller — fungsi MURNI, tanpa
// I/O, tanpa server.
//
// Kontrak yang diimplementasikan (harus lolos SEMUA):
//  1. Nominal persis: amount (integer) == total_snapshot, tanpa float
//     dan tanpa toleransi.
//     1b. CEK IDENTITAS MDR (2026-10-28, atas permintaan Hanif): MDR
//     adalah identitas order. GoBiz TIDAK mengirim field MDR, jadi
//     MDR mutasi diturunkan: mdr_tersirat = amount - price - fee.
//     Bila order mengirim price, fee, dan mdr, pasangan hanya menang
//     bila mdr_tersirat == mdr order; bila tidak sama, alasannya
//     "mdr-tidak-cocok" dan pasangan itu tidak diusulkan walau
//     amount-nya sama dengan total_snapshot. Bila order tidak
//     mengirim rincian itu, cek ini dilewati (mdr_cocok nil) dan
//     pencocokan kembali ke total_snapshot saja — MDR tidak dikarang.
//  2. Rentang waktu: created_at <= transacted_at <= grace_end_at,
//     dengan grace_end_at = created_at + 75 menit (60 menit expiry +
//     15 menit grace).
//  3. Klaim sekali per transaksi: satu transaction_id hanya diusulkan
//     untuk SATU order. Bila cocok dengan >1 order, hanya order dengan
//     created_at paling awal yang diusulkan; sisanya TIDAK_DIUSULKAN
//     dengan alasan "sudah-diklaim". Transaksi yang sudah ada di
//     claimed tidak diusulkan lagi pada siklus berikutnya.
//  4. Kedaluwarsa: order lewat grace tidak diusulkan. Dinilai dari
//     waktu transaksi (transacted_at > grace_end_at) — dan, bila
//     pemanggil mengisi Now, juga dari jam evaluasi (now >
//     grace_end_at -> "kedaluwarsa"). Server sengaja TIDAK mengisi
//     Now agar data dummy bertanggal contoh tetap bisa didemo
//     kapan pun; pemakaian Now didokumentasikan di README.
//  5. Hanya status PENDING: order dengan status lain diabaikan
//     (bukan error). Bila tidak ada order PENDING sama sekali,
//     polling dilewati: tidak ada proposal, satu entri POLLING_DILEWATI.
//
// Input tidak valid dilewati PER PASANGAN / PER ITEM, tidak pernah
// membuat seluruh siklus crash: item yang tidak valid menghasilkan
// entri ERROR ("input-tidak-valid") dan pasangan lain tetap dinilai.
package match

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"mitrain-gopay/internal/types"
)

// MatchRule adalah penanda aturan yang lolos (di proposal).
const MatchRule = "nominal-persis + dalam-rentang + belum-diklaim"

// NormalIntervalMs adalah interval normal antar siklus sukses (60 detik).
const NormalIntervalMs = int64(60_000)

// BackoffSequenceMs adalah urutan backoff error: 60s, 120s, 240s, 300s.
var BackoffSequenceMs = []int64{60_000, 120_000, 240_000, 300_000}

// NextBackoffMs adalah jeda berikutnya setelah consecutiveFailures
// kegagalan beruntun. 0 kegagalan -> interval normal 60 detik.
func NextBackoffMs(consecutiveFailures int64) int64 {
	if consecutiveFailures <= 0 {
		return NormalIntervalMs
	}
	idx := consecutiveFailures - 1
	if idx >= int64(len(BackoffSequenceMs)) {
		idx = int64(len(BackoffSequenceMs)) - 1
	}
	return BackoffSequenceMs[idx]
}

// GraceEndFromCreated menghitung grace_end_at = created_at + 75 menit.
// Bila created_at tidak bisa diparse, kembalikan created_at apa adanya.
func GraceEndFromCreated(createdAt string) string {
	t, err := parseTime(createdAt)
	if err != nil {
		return createdAt
	}
	return t.Add(75 * time.Minute).UTC().Format(time.RFC3339)
}

// MatchOptions adalah opsi evaluasi pencocokan.
type MatchOptions struct {
	// Now adalah jam evaluasi. Bila diisi dan sudah lewat grace_end_at
	// sebuah order, pasangan order itu dinilai "kedaluwarsa" walau
	// transaksinya sendiri masih dalam rentang. Bila nil, kedaluwarsa
	// hanya dinilai dari waktu transaksi.
	Now *time.Time
	// At adalah waktu yang dipakai untuk logged_at / matched_at
	// (default: sekarang).
	At *time.Time
}

// MatchResult adalah hasil satu siklus pencocokan.
type MatchResult struct {
	Proposals      []types.Proposal
	Decisions      []types.AuditLogEntry
	Claimed        map[string]bool
	SkippedPolling bool
}

type validOrder struct {
	types.OrderPending
	createdMs time.Time
	graceMs   time.Time
}

type validMutation struct {
	types.Mutasi
	transactedMs time.Time
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errInvalidTime
}

var errInvalidTime = &parseError{"waktu tidak valid"}

type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }

// parseRawInt menerima angka JSON apa adanya: json.Number, float64
// integer, int, int64. String angka, float, negatif -> tidak valid.
func parseRawInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil && i >= 0
	case float64:
		if math.Trunc(n) != n || n < 0 || n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), n >= 0
	case int64:
		return n, n >= 0
	default:
		return 0, false
	}
}

func rawString(m map[string]any, field string) (string, bool) {
	v, ok := m[field]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok && len(s) > 0
}

func optionalRawInt(m map[string]any, field string) (*int64, bool) {
	v, ok := m[field]
	if !ok || v == nil {
		return nil, true
	}
	n, valid := parseRawInt(v)
	if !valid {
		return nil, false
	}
	return &n, true
}

func validateOrderRaw(raw any) (*validOrder, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	orderID, ok := rawString(m, "order_id")
	if !ok {
		return nil, false
	}
	total, ok := parseRawInt(m["total_snapshot"])
	if !ok {
		return nil, false
	}
	createdStr, ok := rawString(m, "created_at")
	if !ok {
		return nil, false
	}
	createdMs, err := parseTime(createdStr)
	if err != nil {
		return nil, false
	}
	graceStr, ok := rawString(m, "grace_end_at")
	if !ok {
		return nil, false
	}
	graceMs, err := parseTime(graceStr)
	if err != nil {
		return nil, false
	}
	expiryStr, ok := rawString(m, "expiry_at")
	if !ok {
		return nil, false
	}
	if _, err := parseTime(expiryStr); err != nil {
		return nil, false
	}
	status, ok := m["status"].(string)
	if !ok {
		return nil, false
	}
	price, ok := optionalRawInt(m, "price")
	if !ok {
		return nil, false
	}
	fee, ok := optionalRawInt(m, "fee")
	if !ok {
		return nil, false
	}
	mdr, ok := optionalRawInt(m, "mdr")
	if !ok {
		return nil, false
	}
	return &validOrder{
		OrderPending: types.OrderPending{
			OrderID: orderID, TotalSnapshot: total, CreatedAt: createdStr,
			ExpiryAt: expiryStr, GraceEndAt: graceStr, Status: status,
			Price: price, Fee: fee, MDR: mdr,
		},
		createdMs: createdMs,
		graceMs:   graceMs,
	}, true
}

func validateMutationRaw(raw any) (*validMutation, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	txID, ok := rawString(m, "transaction_id")
	if !ok {
		return nil, false
	}
	amount, ok := parseRawInt(m["amount"])
	if !ok {
		return nil, false
	}
	transactedStr, ok := rawString(m, "transacted_at")
	if !ok {
		return nil, false
	}
	transactedMs, err := parseTime(transactedStr)
	if err != nil {
		return nil, false
	}
	source, ok := m["source"].(string)
	if !ok || len(source) == 0 {
		return nil, false
	}
	statusStr, _ := rawString(m, "status")
	paymentTypeStr, _ := rawString(m, "payment_type")
	return &validMutation{
		Mutasi: types.Mutasi{
			TransactionID: txID, Amount: amount, TransactedAt: transactedStr, Source: source,
			Status: statusStr, PaymentType: paymentTypeStr,
		},
		transactedMs: transactedMs,
	}, true
}

func rawID(raw any, field string) *string {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if s, ok := rawString(m, field); ok {
		return types.StrPtr(s)
	}
	return nil
}

func rawAmount(raw any) *int64 {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if n, ok := parseRawInt(m["amount"]); ok {
		return types.IntPtr(n)
	}
	return nil
}

func rawTransactedAt(raw any) *string {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	s, ok := rawString(m, "transacted_at")
	if !ok {
		return nil
	}
	if _, err := parseTime(s); err != nil {
		return nil
	}
	return types.StrPtr(s)
}

// mdrFields menghitung cek identitas MDR untuk satu pasangan.
// GoBiz tidak mengirim MDR, jadi MDR mutasi diturunkan dari amount
// bila harga dan fee order diketahui: mdr_tersirat = amount - price - fee.
func mdrFields(o *validOrder, m *validMutation) (mdrOrder, mdrTersirat *int64, mdrCocok *bool) {
	mdrOrder = o.MDR
	if o.Price != nil && o.Fee != nil {
		v := m.Amount - *o.Price - *o.Fee
		mdrTersirat = types.IntPtr(v)
	}
	if mdrOrder != nil && mdrTersirat != nil {
		cocok := *mdrTersirat == *mdrOrder
		mdrCocok = types.BoolPtr(cocok)
	}
	return mdrOrder, mdrTersirat, mdrCocok
}

// MatchRaw menilai pasangan dari data mentah (hasil decode JSON).
// Fungsi ini TIDAK mengubah claimed masukan; ia mengembalikan map baru.
func MatchRaw(ordersRaw, mutationsRaw []any, claimedSet map[string]bool, opts MatchOptions) MatchResult {
	at := time.Now().UTC()
	if opts.At != nil {
		at = *opts.At
	}
	loggedAt := at.Format(time.RFC3339)
	var nowMs *time.Time
	if opts.Now != nil {
		nowMs = opts.Now
	}
	claimed := make(map[string]bool, len(claimedSet))
	for k, v := range claimedSet {
		claimed[k] = v
	}
	var proposals []types.Proposal
	var decisions []types.AuditLogEntry

	push := func(orderID, txID *string, decision types.Decision, reason types.Reason, amount *int64, transactedAt *string, mdrOrder, mdrTersirat *int64, mdrCocok *bool, status, paymentType string) {
		decisions = append(decisions, types.AuditLogEntry{
			LoggedAt: loggedAt, OrderID: orderID, TransactionID: txID,
			Decision: decision, Reason: reason, Amount: amount,
			TransactedAt: transactedAt, MDROrder: mdrOrder,
			MDRTersirat: mdrTersirat, MDRCocok: mdrCocok,
			Status: status, PaymentType: paymentType,
		})
	}

	// 1. Validasi order. Status non-PENDING diabaikan (bukan error).
	var pending []*validOrder
	for _, raw := range ordersRaw {
		valid, ok := validateOrderRaw(raw)
		if !ok {
			status := ""
			if m, isMap := raw.(map[string]any); isMap {
				if s, isStr := m["status"].(string); isStr {
					status = s
				} else {
					// Status tidak bisa dipastikan -> laporkan sebagai error.
					push(rawID(raw, "order_id"), nil, types.DecisionError, types.ReasonInvalidInput, nil, nil, nil, nil, nil, "", "")
					continue
				}
			}
			if status == "" || status == "PENDING" {
				push(rawID(raw, "order_id"), nil, types.DecisionError, types.ReasonInvalidInput, nil, nil, nil, nil, nil, "", "")
			}
			continue
		}
		if valid.Status == "PENDING" {
			pending = append(pending, valid)
		}
	}

	// 2. Tidak ada order PENDING -> polling dilewati, mutasi tidak dinilai.
	if len(pending) == 0 {
		push(nil, nil, types.DecisionSkipped, types.ReasonNoPending, nil, nil, nil, nil, nil, "", "")
		return MatchResult{Proposals: proposals, Decisions: decisions, Claimed: claimed, SkippedPolling: true}
	}

	// 3. Validasi mutasi. Yang tidak valid dicatat ERROR, tidak crash.
	var validMutations []*validMutation
	for _, raw := range mutationsRaw {
		valid, ok := validateMutationRaw(raw)
		if !ok {
			push(nil, rawID(raw, "transaction_id"), types.DecisionError, types.ReasonInvalidInput, rawAmount(raw), rawTransactedAt(raw), nil, nil, nil, "", "")
			continue
		}
		validMutations = append(validMutations, valid)
	}

	// Urutan kandidat: created_at paling awal menang; tie-break order_id.
	sorted := append([]*validOrder{}, pending...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].createdMs.Equal(sorted[j].createdMs) {
			return sorted[i].createdMs.Before(sorted[j].createdMs)
		}
		return sorted[i].OrderID < sorted[j].OrderID
	})

	// 4. Nilai setiap mutasi terhadap semua order kandidat.
	for _, m := range validMutations {
		var winner *validOrder
		for _, o := range sorted {
			if claimed[m.TransactionID] {
				break
			}
			if m.Amount != o.TotalSnapshot {
				continue
			}
			if m.transactedMs.Before(o.createdMs) || m.transactedMs.After(o.graceMs) {
				continue
			}
			if nowMs != nil && nowMs.After(o.graceMs) {
				continue
			}
			// CEK IDENTITAS MDR EKSPLISIT.
			if _, _, cocok := mdrFields(o, m); cocok != nil && !*cocok {
				continue
			}
			winner = o
			break
		}

		if winner != nil {
			claimed[m.TransactionID] = true
			proposals = append(proposals, types.Proposal{
				OrderID: winner.OrderID, TransactionID: m.TransactionID,
				Amount: m.Amount, TransactedAt: m.TransactedAt,
				ProposalStatus: "DIUSULKAN", MatchedAt: loggedAt, MatchRule: MatchRule,
				Status: m.Status, PaymentType: m.PaymentType,
			})
		}

		for _, o := range sorted {
			mdrOrder, mdrTersirat, mdrCocok := mdrFields(o, m)
			if winner != nil && o.OrderID == winner.OrderID {
				push(types.StrPtr(o.OrderID), types.StrPtr(m.TransactionID), types.DecisionProposed, types.ReasonExactAmount, types.IntPtr(m.Amount), types.StrPtr(m.TransactedAt), mdrOrder, mdrTersirat, mdrCocok, m.Status, m.PaymentType)
				continue
			}
			var reason types.Reason
			if mdrCocok != nil && !*mdrCocok && m.Amount == o.TotalSnapshot {
				// Amount sama dengan total tapi MDR tersiratnya beda:
				// identitasnya tidak cocok, bukan nominalnya yang salah.
				reason = types.ReasonMDRMismatch
			} else if claimed[m.TransactionID] {
				if m.Amount == o.TotalSnapshot &&
					!m.transactedMs.Before(o.createdMs) && !m.transactedMs.After(o.graceMs) &&
					!(nowMs != nil && nowMs.After(o.graceMs)) {
					reason = types.ReasonAlreadyClaimed
				} else if m.Amount != o.TotalSnapshot {
					reason = types.ReasonAmountMismatch
				} else if m.transactedMs.Before(o.createdMs) || m.transactedMs.After(o.graceMs) {
					reason = types.ReasonOutOfWindow
				} else {
					reason = types.ReasonExpired
				}
			} else if m.Amount != o.TotalSnapshot {
				reason = types.ReasonAmountMismatch
			} else if m.transactedMs.Before(o.createdMs) || m.transactedMs.After(o.graceMs) {
				reason = types.ReasonOutOfWindow
			} else if nowMs != nil && nowMs.After(o.graceMs) {
				reason = types.ReasonExpired
			} else {
				reason = types.ReasonNoMatch
			}
			push(types.StrPtr(o.OrderID), types.StrPtr(m.TransactionID), types.DecisionNotProposed, reason, types.IntPtr(m.Amount), types.StrPtr(m.TransactedAt), mdrOrder, mdrTersirat, mdrCocok, m.Status, m.PaymentType)
		}
	}

	return MatchResult{Proposals: proposals, Decisions: decisions, Claimed: claimed, SkippedPolling: false}
}

// MatchOrders adalah versi bertipe dari MatchRaw: order/mutasi yang
// sudah bertipe diubah ke bentuk mentah lalu dinilai dengan aturan
// yang sama persis (termasuk cek identitas MDR).
func MatchOrders(orders []types.OrderPending, mutations []types.Mutasi, claimedSet map[string]bool, opts MatchOptions) MatchResult {
	ordersRaw := make([]any, 0, len(orders))
	for _, o := range orders {
		m := map[string]any{
			"order_id": o.OrderID, "total_snapshot": o.TotalSnapshot,
			"created_at": o.CreatedAt, "expiry_at": o.ExpiryAt,
			"grace_end_at": o.GraceEndAt, "status": o.Status,
		}
		if o.Price != nil {
			m["price"] = *o.Price
		}
		if o.Fee != nil {
			m["fee"] = *o.Fee
		}
		if o.MDR != nil {
			m["mdr"] = *o.MDR
		}
		ordersRaw = append(ordersRaw, m)
	}
	mutationsRaw := make([]any, 0, len(mutations))
	for _, mu := range mutations {
		mut := map[string]any{
			"transaction_id": mu.TransactionID, "amount": mu.Amount,
			"transacted_at": mu.TransactedAt, "source": mu.Source,
		}
		if mu.Status != "" {
			mut["status"] = mu.Status
		}
		if mu.PaymentType != "" {
			mut["payment_type"] = mu.PaymentType
		}
		mutationsRaw = append(mutationsRaw, mut)
	}
	return MatchRaw(ordersRaw, mutationsRaw, claimedSet, opts)
}
