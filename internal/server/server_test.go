package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopay-merchant-go/internal/config"
	"gopay-merchant-go/internal/gobiz"
	"gopay-merchant-go/internal/store"
)

func testConfig(t *testing.T, dataDir string) config.ServerConfig {
	t.Helper()
	return config.ServerConfig{
		Port: 3101, PollIntervalMs: 60000, DataDir: dataDir,
		PollEnabled: false, Source: "mock",
		Gobiz: gobiz.Config{
			BaseURL: "https://api.gojekapi.com", AuthBaseURL: "https://api.gobiz.co.id",
			MerchantID:       "MERCHANT-RAHASIA-TEST",
			SessionFile:      filepath.Join(t.TempDir(), "tidak-ada-sesi.json"),
			TransactionsPath: "/merchant-analytics/v2/merchants/transactions",
			OtpPath:          "/goid/login/request", VerifyPath: "/goid/token",
			RefreshPath: "/goid/token", MerchantConfigPath: "/goresto/v5/public/users/config",
		},
		PollerInternalToken: "TOKEN-INTERNAL-TEST",
		MDRDurationMs:       900000, DatabaseURL: "",
		UIPath: "../../web/ui.html",
	}
}

func writeDummyData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orders := `[{"order_id":"ORDER-DUMMY-001","total_snapshot":1003000,"created_at":"2026-10-28T10:00:00+07:00","expiry_at":"2026-10-28T11:00:00+07:00","grace_end_at":"2026-10-28T11:15:00+07:00","status":"PENDING"},{"order_id":"ORDER-DUMMY-002","total_snapshot":1003000,"created_at":"2026-10-28T10:05:00+07:00","expiry_at":"2026-10-28T11:05:00+07:00","grace_end_at":"2026-10-28T11:20:00+07:00","status":"PENDING"},{"order_id":"ORDER-DUMMY-003","total_snapshot":500000,"created_at":"2026-10-28T10:00:00+07:00","expiry_at":"2026-10-28T11:00:00+07:00","grace_end_at":"2026-10-28T11:15:00+07:00","status":"PENDING"}]`
	mutations := `[{"transaction_id":"TX-DUMMY-001","amount":1003000,"transacted_at":"2026-10-28T10:10:00+07:00","source":"mock-gobiz-lokal"},{"transaction_id":"TX-DUMMY-002","amount":500000,"transacted_at":"2026-10-28T10:20:00+07:00","source":"mock-gobiz-lokal"}]`
	if err := os.WriteFile(filepath.Join(dir, "orders.dummy.json"), []byte(orders), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mutations.dummy.json"), []byte(mutations), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func doRequest(t *testing.T, srv *Server, method, path string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func TestHealthDanUI(t *testing.T) {
	srv := NewServer(testConfig(t, writeDummyData(t)), store.NewMemoryStore())
	status, body := doRequest(t, srv, http.MethodGet, "/health", nil, nil)
	if status != 200 || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("health = %d %s, mau 200 ok", status, body)
	}
	status, body = doRequest(t, srv, http.MethodGet, "/", nil, nil)
	if status != 200 || len(body) < 1000 {
		t.Fatalf("UI = %d len %d, mau 200 dan isi ui.html", status, len(body))
	}
	status, _ = doRequest(t, srv, http.MethodGet, "/tidak-ada", nil, nil)
	if status != 404 {
		t.Fatalf("route tidak dikenal = %d, mau 404", status)
	}
}

func TestUIConfigHanyaFieldAman(t *testing.T) {
	srv := NewServer(testConfig(t, writeDummyData(t)), store.NewMemoryStore())
	status, body := doRequest(t, srv, http.MethodGet, "/ui/config", nil, nil)
	if status != 200 {
		t.Fatalf("ui/config = %d %s", status, body)
	}
	for _, forbidden := range []string{"TOKEN-INTERNAL-TEST", "MERCHANT-RAHASIA-TEST", "tidak-ada-sesi.json", "DATABASE_URL"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("ui/config mengandung %q, tidak boleh (hanya field aman)", forbidden)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("ui/config bukan JSON: %v", err)
	}
	if parsed["source"] != "mock" || parsed["port"] != float64(3101) || parsed["db_configured"] != false || parsed["session_file_exists"] != false {
		t.Fatalf("ui/config = %+v, mau source mock port 3101 db false session false", parsed)
	}
}

func TestProposalsMockDuaLaluNol(t *testing.T) {
	srv := NewServer(testConfig(t, writeDummyData(t)), store.NewMemoryStore())
	status, body := doRequest(t, srv, http.MethodGet, "/proposals", nil, nil)
	if status != 200 {
		t.Fatalf("proposals pertama = %d %s", status, body)
	}
	var first map[string]any
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatalf("proposals bukan JSON: %v", err)
	}
	proposals, _ := first["proposals"].([]any)
	if len(proposals) != 2 {
		t.Fatalf("proposals pertama = %d, mau 2 (DUMMY-001 + DUMMY-003; DUMMY-002 kalah klaim)", len(proposals))
	}
	status, body = doRequest(t, srv, http.MethodGet, "/proposals", nil, nil)
	if status != 200 {
		t.Fatalf("proposals kedua = %d %s", status, body)
	}
	var second map[string]any
	_ = json.Unmarshal(body, &second)
	proposals2, _ := second["proposals"].([]any)
	if len(proposals2) != 0 {
		t.Fatalf("proposals kedua = %d, mau 0 (klaim sudah maju)", len(proposals2))
	}
	status, body = doRequest(t, srv, http.MethodGet, "/audit", nil, nil)
	if status != 200 || !strings.Contains(string(body), "TX-DUMMY-001") {
		t.Fatalf("audit = %d %s, mau berisi TX-DUMMY-001", status, body)
	}
}

func TestMDRReserveAuth(t *testing.T) {
	// Token kosong -> 500.
	cfg := testConfig(t, writeDummyData(t))
	cfg.PollerInternalToken = ""
	srv := NewServer(cfg, store.NewMemoryStore())
	status, _ := doRequest(t, srv, http.MethodPost, "/mdr/reserve", []byte(`{"order_id":"ORDER-X","price":1000}`), map[string]string{"Content-Type": "application/json"})
	if status != 500 {
		t.Fatalf("token kosong = %d, mau 500", status)
	}
	// Token salah -> 401.
	srv = NewServer(testConfig(t, writeDummyData(t)), store.NewMemoryStore())
	status, _ = doRequest(t, srv, http.MethodPost, "/mdr/reserve", []byte(`{"order_id":"ORDER-X","price":1000}`), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer SALAH"})
	if status != 401 {
		t.Fatalf("token salah = %d, mau 401", status)
	}
	// Token benar -> 200, total = harga + fee + kode unik 199-1999.
	status, body := doRequest(t, srv, http.MethodPost, "/mdr/reserve", []byte(`{"order_id":"ORDER-X","price":1000}`), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer TOKEN-INTERNAL-TEST"})
	if status != 200 {
		t.Fatalf("token benar = %d %s, mau 200", status, body)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	total := parsed["total"].(float64)
	unique := parsed["unique_code"].(float64)
	if total != 1000+unique || unique < 199 || unique > 1999 {
		t.Fatalf("reserve = %+v, mau total = 1000 + kode unik 199-1999", parsed)
	}
	// Idempotent: order sama lagi -> total sama.
	status, body2 := doRequest(t, srv, http.MethodPost, "/mdr/reserve", []byte(`{"order_id":"ORDER-X","price":1000}`), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer TOKEN-INTERNAL-TEST"})
	if status != 200 {
		t.Fatalf("reserve ulang = %d %s", status, body2)
	}
	var parsed2 map[string]any
	_ = json.Unmarshal(body2, &parsed2)
	if parsed2["total"] != total {
		t.Fatalf("reserve ulang total = %v, mau sama %v", parsed2["total"], total)
	}
	// /ui/reserve tanpa token (localhost) juga 200.
	status, _ = doRequest(t, srv, http.MethodPost, "/ui/reserve", []byte(`{"order_id":"ORDER-Y","price":1000}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("ui/reserve = %d, mau 200", status)
	}
	// Body tidak valid -> 400.
	status, _ = doRequest(t, srv, http.MethodPost, "/mdr/reserve", []byte(`{"order_id":"","price":0}`), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer TOKEN-INTERNAL-TEST"})
	if status != 400 {
		t.Fatalf("body tidak valid = %d, mau 400", status)
	}
}

func TestMutationsRiwayatBacaSaja(t *testing.T) {
	srv := NewServer(testConfig(t, writeDummyData(t)), store.NewMemoryStore())
	// Tanpa filter: 2 mutasi dummy, terbaru dulu (TX-DUMMY-002 10:20 di atas 001 10:10).
	status, body := doRequest(t, srv, http.MethodGet, "/mutations", nil, nil)
	if status != 200 {
		t.Fatalf("mutations = %d %s", status, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("mutations bukan JSON: %v", err)
	}
	if parsed["count"] != float64(2) || parsed["source"] != "mock" {
		t.Fatalf("mutations = %+v, mau count 2 source mock", parsed)
	}
	list, _ := parsed["mutations"].([]any)
	if len(list) != 2 {
		t.Fatalf("mutations list = %d, mau 2", len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["transaction_id"] != "TX-DUMMY-002" || first["amount"] != float64(500000) {
		t.Fatalf("mutasi pertama = %+v, mau TX-DUMMY-002 500000 (terbaru dulu)", first)
	}
	// Filter rentang: dari 10:15 -> hanya TX-DUMMY-002.
	status, body = doRequest(t, srv, http.MethodGet, "/mutations?start_time=2026-10-28T10:15:00%2B07:00", nil, nil)
	if status != 200 {
		t.Fatalf("mutations filter = %d %s", status, body)
	}
	var filtered map[string]any
	_ = json.Unmarshal(body, &filtered)
	if filtered["count"] != float64(1) {
		t.Fatalf("mutations filter = %+v, mau count 1", filtered)
	}
	// Waktu tidak valid -> 400.
	status, _ = doRequest(t, srv, http.MethodGet, "/mutations?start_time=bukan-waktu", nil, nil)
	if status != 400 {
		t.Fatalf("mutations waktu salah = %d, mau 400", status)
	}
	// SOURCE tidak valid -> 500, tanpa fallback.
	cfg := testConfig(t, writeDummyData(t))
	cfg.Source = "aneh"
	srv2 := NewServer(cfg, store.NewMemoryStore())
	status, body = doRequest(t, srv2, http.MethodGet, "/mutations", nil, nil)
	if status != 500 || !strings.Contains(string(body), "konfigurasi-tidak-valid") {
		t.Fatalf("mutations SOURCE aneh = %d %s, mau 500 konfigurasi-tidak-valid", status, body)
	}
	// BACA SAJA: /mutations tidak boleh memajukan klaim — /proposals
	// setelahnya masih mengusulkan 2 (bukan 0).
	status, body = doRequest(t, srv, http.MethodGet, "/proposals", nil, nil)
	if status != 200 {
		t.Fatalf("proposals setelah mutations = %d %s", status, body)
	}
	var prop map[string]any
	_ = json.Unmarshal(body, &prop)
	proposals, _ := prop["proposals"].([]any)
	if len(proposals) != 2 {
		t.Fatalf("proposals setelah mutations = %d, mau 2 (klaim tidak maju)", len(proposals))
	}
	// UI memuat kartu riwayat.
	status, body = doRequest(t, srv, http.MethodGet, "/", nil, nil)
	if status != 200 || !strings.Contains(string(body), "Riwayat pembayaran GoBiz") || !strings.Contains(string(body), "mutations-body") {
		t.Fatalf("UI = %d, mau memuat kartu Riwayat pembayaran GoBiz", status)
	}
}

func TestMutationsGobizTanpaRentangJadi24Jam(t *testing.T) {
	// Server GoBiz tiruan: catat query start/end, kembalikan 1 mutasi.
	var gotStart, gotEnd string
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStart = r.URL.Query().Get("start_time")
		gotEnd = r.URL.Query().Get("end_time")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"transaction_id":"TX-GOBIZ-001","amount":278000,"transacted_at":"2026-10-29T12:20:00+07:00"}]}`))
	}))
	defer mockSrv.Close()
	dir := t.TempDir()
	sesi := filepath.Join(dir, "sesi.json")
	if err := os.WriteFile(sesi, []byte(`{"accessToken":"ACCESS-TEST","refreshToken":"REFRESH-TEST","expiresAt":"2099-01-01T00:00:00Z","merchantId":"MERCHANT-TEST"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, writeDummyData(t))
	cfg.Source = "gobiz"
	cfg.Gobiz.BaseURL = mockSrv.URL
	cfg.Gobiz.SessionFile = sesi
	cfg.Gobiz.MerchantID = "MERCHANT-TEST"
	srv := NewServer(cfg, store.NewMemoryStore())
	// Tanpa query rentang sama sekali.
	status, body := doRequest(t, srv, http.MethodGet, "/mutations", nil, nil)
	if status != 200 {
		t.Fatalf("mutations gobiz tanpa rentang = %d %s", status, body)
	}
	if gotStart == "" || gotEnd == "" {
		t.Fatalf("query ke GoBiz start=%q end=%q, mau keduanya terisi (default 24 jam)", gotStart, gotEnd)
	}
	if _, err := time.Parse(time.RFC3339, gotStart); err != nil {
		t.Fatalf("start_time ke GoBiz = %q, mau RFC3339", gotStart)
	}
	if _, err := time.Parse(time.RFC3339, gotEnd); err != nil {
		t.Fatalf("end_time ke GoBiz = %q, mau RFC3339", gotEnd)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if parsed["count"] != float64(1) {
		t.Fatalf("mutations gobiz = %+v, mau count 1", parsed)
	}
	list, _ := parsed["mutations"].([]any)
	first, _ := list[0].(map[string]any)
	if first["transaction_id"] != "TX-GOBIZ-001" || first["amount"] != float64(2780) {
		t.Fatalf("mutasi gobiz = %+v, mau TX-GOBIZ-001 2780", first)
	}
	// Respons harus menampilkan rentang efektif yang dipakai.
	if parsed["start_time"] != gotStart || parsed["end_time"] != gotEnd {
		t.Fatalf("rentang respons = %v s.d. %v, mau sama dengan yang dikirim ke GoBiz %q s.d. %q", parsed["start_time"], parsed["end_time"], gotStart, gotEnd)
	}
}

func TestMutationsGobizErrorMembawaHTTPStatus(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"invalid"}`))
	}))
	defer mockSrv.Close()
	dir := t.TempDir()
	sesi := filepath.Join(dir, "sesi.json")
	if err := os.WriteFile(sesi, []byte(`{"accessToken":"ACCESS-TEST","refreshToken":"REFRESH-TEST","expiresAt":"2099-01-01T00:00:00Z","merchantId":"MERCHANT-TEST"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, writeDummyData(t))
	cfg.Source = "gobiz"
	cfg.Gobiz.BaseURL = mockSrv.URL
	cfg.Gobiz.SessionFile = sesi
	cfg.Gobiz.MerchantID = "MERCHANT-TEST"
	srv := NewServer(cfg, store.NewMemoryStore())
	status, body := doRequest(t, srv, http.MethodGet, "/mutations", nil, nil)
	if status != 500 {
		t.Fatalf("mutations gobiz 422 = %d %s, mau 500", status, body)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if parsed["gobiz_http_status"] != float64(422) {
		t.Fatalf("mutations gobiz error = %+v, mau gobiz_http_status 422", parsed)
	}
}

func TestProposalsSourceTidakValid(t *testing.T) {
	cfg := testConfig(t, writeDummyData(t))
	cfg.Source = "aneh"
	srv := NewServer(cfg, store.NewMemoryStore())
	status, body := doRequest(t, srv, http.MethodGet, "/proposals", nil, nil)
	if status != 500 || !strings.Contains(string(body), "konfigurasi-tidak-valid") {
		t.Fatalf("SOURCE aneh = %d %s, mau 500 konfigurasi-tidak-valid", status, body)
	}
}

func TestNotifyOrder(t *testing.T) {
	srv := NewServer(testConfig(t, writeDummyData(t)), store.NewMemoryStore())
	// Total tidak sama dengan price+mdr+fee -> 400.
	status, _ := doRequest(t, srv, http.MethodPost, "/notify-order", []byte(`{"order_id":"ORDER-PUSH-001","total":9999,"price":12000,"mdr":876,"fee":0}`), map[string]string{"Content-Type": "application/json"})
	if status != 400 {
		t.Fatalf("notify total tidak konsisten = %d, mau 400", status)
	}
	// Push valid -> 200, ok true, dan tercatat di /notified-orders.
	status, body := doRequest(t, srv, http.MethodPost, "/notify-order", []byte(`{"order_id":"ORDER-PUSH-001","total":12876,"price":12000,"mdr":876,"fee":0,"credit_amount":10000,"expires_at":"2026-10-29T08:08:50Z","created_at":"2026-10-29T07:08:50Z"}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("notify valid = %d %s, mau 200 ok", status, body)
	}
	status, body = doRequest(t, srv, http.MethodGet, "/notified-orders", nil, nil)
	if status != 200 || !strings.Contains(string(body), "ORDER-PUSH-001") {
		t.Fatalf("notified-orders = %d %s, mau memuat ORDER-PUSH-001", status, body)
	}
	// Push ulang order yang sama (idempotent) -> tetap 200, tetap 1 entri.
	status, _ = doRequest(t, srv, http.MethodPost, "/notify-order", []byte(`{"order_id":"ORDER-PUSH-001","total":12876,"price":12000,"mdr":876,"fee":0}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("notify ulang = %d, mau 200", status)
	}
	status, body = doRequest(t, srv, http.MethodGet, "/notified-orders", nil, nil)
	if status != 200 || !strings.Contains(string(body), `"count":1`) {
		t.Fatalf("notified-orders setelah push ulang = %d %s, mau count 1", status, body)
	}
}

func TestNotifyOrderMembangunkanLoopInternal(t *testing.T) {
	srv := NewServer(testConfig(t, writeDummyData(t)), store.NewMemoryStore())
	wake := make(chan struct{}, 1)
	srv.SetWireWake(wake)
	status, body := doRequest(t, srv, http.MethodPost, "/notify-order", []byte(`{"order_id":"ORDER-WAKE-001","total":12876,"price":12000,"mdr":876,"fee":0}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 || !strings.Contains(string(body), `"triggered":true`) {
		t.Fatalf("notify dengan loop internal = %d %s, mau 200 triggered true", status, body)
	}
	select {
	case <-wake:
	default:
		t.Fatal("notify tidak mengirim sinyal bangun ke loop internal")
	}
}
