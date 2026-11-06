package wire

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopay-merchant-go/internal/gobiz"
	"gopay-merchant-go/internal/types"
	"gopay-merchant-go/internal/wirelog"
)

func order() types.OrderPending {
	return types.OrderPending{
		OrderID: "ORDER-001", TotalSnapshot: 2216,
		CreatedAt: "2026-10-28T10:00:00+07:00", ExpiryAt: "2026-10-28T11:00:00+07:00",
		GraceEndAt: "2026-10-28T11:15:00+07:00", Status: "PENDING",
		Price: types.IntPtr(1000), Fee: types.IntPtr(0), MDR: types.IntPtr(1216),
	}
}

func mutation(amount int64) types.Mutasi {
	return types.Mutasi{TransactionID: "TX-001", Amount: amount, TransactedAt: "2026-10-28T10:10:00+07:00", Source: "gobiz"}
}

func TestRunWireCycleSettled(t *testing.T) {
	settleCalls := 0
	result, err := RunWireCycle(context.Background(), CycleInput{
		Orders: []types.OrderPending{order()}, Mutations: []types.Mutasi{mutation(2216)},
		Claimed: map[string]bool{},
		Settle: func(ctx context.Context, p types.Proposal) (SettleResult, error) {
			settleCalls++
			return SettleResult{Ok: true, Status: 200, Body: map[string]any{}}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunWireCycle: %v", err)
	}
	if len(result.Settled) != 1 || settleCalls != 1 {
		t.Fatalf("settled = %+v calls = %d, mau 1", result.Settled, settleCalls)
	}
	if !result.Claimed["TX-001"] {
		t.Fatal("TX-001 harus diklaim setelah settled")
	}
	if len(result.Decisions) == 0 {
		t.Fatal("decisions harus dibawa")
	}
	found := false
	for _, d := range result.Decisions {
		if d.Decision == types.DecisionProposed && d.MDRCocok != nil && *d.MDRCocok {
			found = true
		}
	}
	if !found {
		t.Fatalf("decisions = %+v, mau ada DIUSULKAN dengan mdr_cocok true", result.Decisions)
	}
}

func TestRunWireCycleRejectedTidakDiklaim(t *testing.T) {
	inputClaimed := map[string]bool{}
	result, err := RunWireCycle(context.Background(), CycleInput{
		Orders: []types.OrderPending{order()}, Mutations: []types.Mutasi{mutation(2216)},
		Claimed: inputClaimed,
		Settle: func(ctx context.Context, p types.Proposal) (SettleResult, error) {
			return SettleResult{Ok: false, Status: 400, Body: map[string]any{"error": "PAYMENT_AMOUNT_MISMATCH"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunWireCycle: %v", err)
	}
	if len(result.Settled) != 0 || len(result.Rejected) != 1 || result.Rejected[0].Status != 400 {
		t.Fatalf("settled/rejected = %+v/%+v, mau 0/1 status 400", result.Settled, result.Rejected)
	}
	if result.Claimed["TX-001"] {
		t.Fatal("transaksi yang ditolak TIDAK boleh diklaim (bisa dicoba lagi)")
	}
	if inputClaimed["TX-001"] {
		t.Fatal("input claimed tidak boleh diubah")
	}
}

func TestRunWireCycleSkippedTanpaPending(t *testing.T) {
	paid := order()
	paid.Status = "PAID"
	calls := 0
	result, err := RunWireCycle(context.Background(), CycleInput{
		Orders: []types.OrderPending{paid}, Mutations: []types.Mutasi{mutation(2216)},
		Claimed: map[string]bool{},
		Settle: func(ctx context.Context, p types.Proposal) (SettleResult, error) {
			calls++
			return SettleResult{Ok: true, Status: 200}, nil
		},
	})
	if err != nil || !result.SkippedPolling || calls != 0 {
		t.Fatalf("skipped = %v calls = %d err = %v, mau skipped true calls 0", result.SkippedPolling, calls, err)
	}
}

func TestFetchMitraPendingOrdersDanSettle(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/topup/pending-orders", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TOKEN-TEST" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"order_id":"ORDER-001","total_snapshot":2216,"created_at":"2026-10-28T10:00:00+07:00","expiry_at":"2026-10-28T11:00:00+07:00","grace_end_at":"2026-10-28T11:15:00+07:00","status":"PENDING","price":1000,"fee":0,"mdr":1216}]`))
	})
	mux.HandleFunc("/api/internal/topup/settlements", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body["amount"] == float64(2216) {
			_, _ = w.Write([]byte(`{"status":"PAID"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"PAYMENT_AMOUNT_MISMATCH"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	orders, err := FetchMitraPendingOrders(ctx, nil, srv.URL, "TOKEN-TEST")
	if err != nil || len(orders) != 1 || orders[0].MDR == nil || *orders[0].MDR != 1216 {
		t.Fatalf("pending = %+v err = %v, mau 1 order MDR 1216", orders, err)
	}
	if _, err := FetchMitraPendingOrders(ctx, nil, srv.URL, "TOKEN-SALAH"); err == nil {
		t.Fatal("token salah harus error (tanpa menampilkan token di pesan)")
	}
	res, err := SettleMitraOrder(ctx, nil, srv.URL, "TOKEN-TEST", SettleInput{OrderID: "ORDER-001", TransactionID: "TX-001", Amount: 2216, TransactedAt: "2026-10-28T10:10:00+07:00"})
	if err != nil || !res.Ok || res.Status != 200 {
		t.Fatalf("settle = %+v err = %v, mau ok 200", res, err)
	}
	res, err = SettleMitraOrder(ctx, nil, srv.URL, "TOKEN-TEST", SettleInput{OrderID: "ORDER-001", TransactionID: "TX-001", Amount: 1000, TransactedAt: "2026-10-28T10:10:00+07:00"})
	if err != nil || res.Ok || res.Status != 400 {
		t.Fatalf("settle salah = %+v err = %v, mau tidak ok 400 (tanpa throw)", res, err)
	}
}

func TestRunLiveWireCycleEndToEndMock(t *testing.T) {
	// Mitra mock.
	mitra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/pending-orders") {
			_, _ = w.Write([]byte(`[{"order_id":"ORDER-001","total_snapshot":2216,"created_at":"2026-10-28T10:00:00+07:00","expiry_at":"2026-10-28T11:00:00+07:00","grace_end_at":"2026-10-28T11:15:00+07:00","status":"PENDING","price":1000,"fee":0,"mdr":1216}]`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"PAID"}`))
	}))
	defer mitra.Close()
	// GoBiz mock: hanya endpoint transaksi yang dipakai live cycle.
	gobizSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"transaction_id":"TX-LIVE-001","amount":221600,"transacted_at":"2026-10-28T10:10:00+07:00"}]}`))
	}))
	defer gobizSrv.Close()
	cfg := gobiz.Config{
		BaseURL: gobizSrv.URL, AuthBaseURL: gobizSrv.URL,
		TransactionsPath: "/transactions",
		OtpPath:          "/goid/login/request", VerifyPath: "/goid/token",
		RefreshPath: "/goid/token", MerchantConfigPath: "/goresto/v5/public/users/config",
	}
	session := &gobiz.Session{
		AccessToken: "ACCESS-TEST", RefreshToken: "REFRESH-TEST",
		ExpiresAt: "2099-01-01T00:00:00Z", MerchantID: "MERCHANT-TEST-001",
	}
	result, err := RunLiveWireCycle(context.Background(), LiveCycleInput{
		MitraBaseURL: mitra.URL, Token: "TOKEN-TEST", GobizConfig: cfg,
		Claimed: map[string]bool{}, Session: session,
	})
	if err != nil {
		t.Fatalf("live cycle: %v", err)
	}
	if len(result.Settled) != 1 || result.Settled[0].TransactionID != "TX-LIVE-001" {
		t.Fatalf("settled = %+v, mau TX-LIVE-001", result.Settled)
	}
	// Backend tidak terjangkau -> error jelas, bukan skipped.
	_, err = RunLiveWireCycle(context.Background(), LiveCycleInput{
		MitraBaseURL: "http://127.0.0.1:1", Token: "TOKEN-TEST", GobizConfig: cfg,
		Claimed: map[string]bool{}, Session: session,
	})
	if err == nil || !strings.Contains(err.Error(), "backend-tidak-terjangkau") {
		t.Fatalf("backend mati: err = %v, mau mengandung backend-tidak-terjangkau", err)
	}
}

func TestWireLogMembawaFieldMDR(t *testing.T) {
	result, err := RunWireCycle(context.Background(), CycleInput{
		Orders: []types.OrderPending{order()}, Mutations: []types.Mutasi{mutation(2216)},
		Claimed: map[string]bool{},
		Settle: func(ctx context.Context, p types.Proposal) (SettleResult, error) {
			return SettleResult{Ok: true, Status: 200}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunWireCycle: %v", err)
	}
	entries := wirelog.DecisionLogEntries(result.Decisions)
	found := false
	for _, e := range entries {
		if e.Decision == types.DecisionProposed && e.MDRCocok != nil && *e.MDRCocok && e.MDROrder != nil && *e.MDROrder == 1216 && e.MDRTersirat != nil && *e.MDRTersirat == 1216 {
			found = true
		}
	}
	if !found {
		t.Fatalf("log entries = %+v, mau ada mdr_order/tersirat 1216 cocok true", entries)
	}
}

func TestNextLoopDelayMs(t *testing.T) {
	cases := []struct {
		in   LoopDelayInput
		want int64
	}{
		{LoopDelayInput{BaseMs: 60000, MaxIdleMs: 300000}, 60000},
		{LoopDelayInput{HadError: true, ConsecutiveFailures: 1, BaseMs: 60000, MaxIdleMs: 300000}, 60000},
		{LoopDelayInput{HadError: true, ConsecutiveFailures: 2, BaseMs: 60000, MaxIdleMs: 300000}, 120000},
		{LoopDelayInput{HadError: true, ConsecutiveFailures: 3, BaseMs: 60000, MaxIdleMs: 300000}, 240000},
		{LoopDelayInput{HadError: true, ConsecutiveFailures: 9, BaseMs: 60000, MaxIdleMs: 300000}, 300000},
		{LoopDelayInput{Skipped: true, ConsecutiveSkipped: 1, BaseMs: 60000, MaxIdleMs: 300000}, 60000},
		{LoopDelayInput{Skipped: true, ConsecutiveSkipped: 2, BaseMs: 60000, MaxIdleMs: 300000}, 60000},
		{LoopDelayInput{Skipped: true, ConsecutiveSkipped: 9, BaseMs: 60000, MaxIdleMs: 300000}, 60000},
	}
	for i, c := range cases {
		if got := NextLoopDelayMs(c.in); got != c.want {
			t.Fatalf("kasus %d: delay = %d, mau %d", i, got, c.want)
		}
	}
	if got := ParseIntervalMs("", 60000); got != 60000 {
		t.Fatalf("ParseIntervalMs kosong = %d, mau 60000", got)
	}
	if got := ParseIntervalMs("abc", 60000); got != 60000 {
		t.Fatalf("ParseIntervalMs abc = %d, mau 60000", got)
	}
	if got := ParseIntervalMs("0", 60000); got != 60000 {
		t.Fatalf("ParseIntervalMs 0 = %d, mau 60000", got)
	}
}
