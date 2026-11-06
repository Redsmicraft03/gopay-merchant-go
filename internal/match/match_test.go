package match

import (
	"testing"
	"time"

	"mitrain-gopay/internal/types"
)

func order(over func(*types.OrderPending)) types.OrderPending {
	o := types.OrderPending{
		OrderID: "ORDER-001", TotalSnapshot: 2216,
		CreatedAt: "2026-10-28T10:00:00+07:00", ExpiryAt: "2026-10-28T11:00:00+07:00",
		GraceEndAt: "2026-10-28T11:15:00+07:00", Status: "PENDING",
		Price: types.IntPtr(1000), Fee: types.IntPtr(0), MDR: types.IntPtr(1216),
	}
	if over != nil {
		over(&o)
	}
	return o
}

func mutation(amount int64, at string) types.Mutasi {
	return types.Mutasi{TransactionID: "TX-001", Amount: amount, TransactedAt: at, Source: "gobiz"}
}

func findDecision(t *testing.T, result MatchResult, txID string) types.AuditLogEntry {
	t.Helper()
	for _, d := range result.Decisions {
		if d.TransactionID != nil && *d.TransactionID == txID {
			return d
		}
	}
	t.Fatalf("keputusan untuk %s tidak ditemukan", txID)
	return types.AuditLogEntry{}
}

func TestNominalPersisDanWindow(t *testing.T) {
	result := MatchOrders([]types.OrderPending{order(nil)}, []types.Mutasi{mutation(2216, "2026-10-28T10:10:00+07:00")}, map[string]bool{}, MatchOptions{})
	if len(result.Proposals) != 1 || result.Proposals[0].OrderID != "ORDER-001" {
		t.Fatalf("proposal = %+v, mau 1 proposal ORDER-001", result.Proposals)
	}
	d := findDecision(t, result, "TX-001")
	if d.Decision != types.DecisionProposed || d.Reason != types.ReasonExactAmount {
		t.Fatalf("keputusan = %v/%v, mau DIUSULKAN/nominal-persis", d.Decision, d.Reason)
	}
	if d.MDRCocok == nil || !*d.MDRCocok || d.MDRTersirat == nil || *d.MDRTersirat != 1216 {
		t.Fatalf("MDR = %+v, mau tersirat 1216 cocok true", d)
	}
}

func TestWindowSebelumDanSesudah(t *testing.T) {
	for _, at := range []string{"2026-10-28T09:59:00+07:00", "2026-10-28T11:16:00+07:00"} {
		result := MatchOrders([]types.OrderPending{order(nil)}, []types.Mutasi{mutation(2216, at)}, map[string]bool{}, MatchOptions{})
		if len(result.Proposals) != 0 {
			t.Fatalf("at %s: proposal = %+v, mau kosong", at, result.Proposals)
		}
		d := findDecision(t, result, "TX-001")
		if d.Reason != types.ReasonOutOfWindow {
			t.Fatalf("at %s: reason = %v, mau di-luar-rentang-waktu", at, d.Reason)
		}
	}
}

func TestKlaimSekaliTerawalMenang(t *testing.T) {
	orders := []types.OrderPending{
		order(func(o *types.OrderPending) { o.OrderID = "ORDER-AWAL" }),
		order(func(o *types.OrderPending) {
			o.OrderID = "ORDER-AKHIR"
			o.CreatedAt = "2026-10-28T10:05:00+07:00"
			o.ExpiryAt = "2026-10-28T11:05:00+07:00"
			o.GraceEndAt = "2026-10-28T11:20:00+07:00"
		}),
	}
	result := MatchOrders(orders, []types.Mutasi{mutation(2216, "2026-10-28T10:10:00+07:00")}, map[string]bool{}, MatchOptions{})
	if len(result.Proposals) != 1 || result.Proposals[0].OrderID != "ORDER-AWAL" {
		t.Fatalf("proposal = %+v, mau hanya ORDER-AWAL", result.Proposals)
	}
	// Transaksi yang sudah diklaim tidak diusulkan lagi pada siklus berikutnya.
	result2 := MatchOrders(orders, []types.Mutasi{mutation(2216, "2026-10-28T10:10:00+07:00")}, result.Claimed, MatchOptions{})
	if len(result2.Proposals) != 0 {
		t.Fatalf("siklus kedua: proposal = %+v, mau kosong", result2.Proposals)
	}
}

func TestKedaluwarsaViaNow(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-10-28T12:00:00+07:00")
	result := MatchOrders([]types.OrderPending{order(nil)}, []types.Mutasi{mutation(2216, "2026-10-28T10:10:00+07:00")}, map[string]bool{}, MatchOptions{Now: &now})
	if len(result.Proposals) != 0 {
		t.Fatalf("proposal = %+v, mau kosong (kedaluwarsa)", result.Proposals)
	}
	d := findDecision(t, result, "TX-001")
	if d.Reason != types.ReasonExpired {
		t.Fatalf("reason = %v, mau kedaluwarsa", d.Reason)
	}
}

func TestTidakAdaPendingDanNonPendingDiabaikan(t *testing.T) {
	paid := order(func(o *types.OrderPending) { o.Status = "PAID" })
	result := MatchOrders([]types.OrderPending{paid}, []types.Mutasi{mutation(2216, "2026-10-28T10:10:00+07:00")}, map[string]bool{}, MatchOptions{})
	if !result.SkippedPolling || len(result.Proposals) != 0 {
		t.Fatalf("skipped = %v proposals = %+v, mau skipped true kosong", result.SkippedPolling, result.Proposals)
	}
	if len(result.Decisions) != 1 || result.Decisions[0].Decision != types.DecisionSkipped {
		t.Fatalf("decisions = %+v, mau 1 POLLING_DILEWATI", result.Decisions)
	}
}

func TestInputTidakValidTidakCrash(t *testing.T) {
	ordersRaw := []any{
		map[string]any{"order_id": "ORDER-BAD", "total_snapshot": "bukan-angka", "created_at": "2026-10-28T10:00:00+07:00", "expiry_at": "2026-10-28T11:00:00+07:00", "grace_end_at": "2026-10-28T11:15:00+07:00", "status": "PENDING"},
		map[string]any{"order_id": "ORDER-OK", "total_snapshot": float64(2216), "created_at": "2026-10-28T10:00:00+07:00", "expiry_at": "2026-10-28T11:00:00+07:00", "grace_end_at": "2026-10-28T11:15:00+07:00", "status": "PENDING"},
	}
	mutationsRaw := []any{
		map[string]any{"transaction_id": "TX-BAD", "amount": "x", "transacted_at": "2026-10-28T10:10:00+07:00", "source": "gobiz"},
		map[string]any{"transaction_id": "TX-OK", "amount": float64(2216), "transacted_at": "2026-10-28T10:10:00+07:00", "source": "gobiz"},
	}
	result := MatchRaw(ordersRaw, mutationsRaw, map[string]bool{}, MatchOptions{})
	if len(result.Proposals) != 1 || result.Proposals[0].OrderID != "ORDER-OK" {
		t.Fatalf("proposal = %+v, mau hanya ORDER-OK", result.Proposals)
	}
	errorCount := 0
	for _, d := range result.Decisions {
		if d.Decision == types.DecisionError {
			errorCount++
		}
	}
	if errorCount != 2 {
		t.Fatalf("error entries = %d, mau 2 (order rusak + mutasi rusak)", errorCount)
	}
}

func TestGraceEndFromCreated(t *testing.T) {
	if got := GraceEndFromCreated("2026-10-28T10:00:00+07:00"); got != "2026-10-28T11:15:00+07:00" && got != "2026-10-28T04:15:00Z" {
		// Format output UTC (Z) juga diterima: bandingkan waktunya.
		a, _ := time.Parse(time.RFC3339, "2026-10-28T11:15:00+07:00")
		b, err := time.Parse(time.RFC3339, got)
		if err != nil || !a.Equal(b) {
			t.Fatalf("grace = %q, mau 11:15+07:00", got)
		}
	}
	if got := GraceEndFromCreated("bukan-waktu"); got != "bukan-waktu" {
		t.Fatalf("input tidak valid harus dikembalikan apa adanya, dapat %q", got)
	}
}
