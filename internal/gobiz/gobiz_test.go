package gobiz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mockGobizServer adalah server tiruan GoBiz lengkap untuk tes.
func mockGobizServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/goid/login/request", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != ClientID || body["country_code"] != "62" {
			t.Errorf("request OTP body = %+v, mau client_id %s country_code 62", body, ClientID)
		}
		if body["phone_number"] != "8123456789" {
			t.Errorf("phone_number = %v, mau 8123456789 (nasional, tanpa 0/62)", body["phone_number"])
		}
		if r.Header.Get("X-Uniqueid") == "" || r.Header.Get("Authentication-Type") != "go-id" {
			t.Errorf("header request OTP tidak lengkap: uniqueid=%q auth-type=%q", r.Header.Get("X-Uniqueid"), r.Header.Get("Authentication-Type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"otp_token":"OTP-TOKEN-TEST","otp_length":4,"otp_expires_in":720}}}`))
	})
	mux.HandleFunc("/goid/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		grant, _ := body["grant_type"].(string)
		w.Header().Set("Content-Type", "application/json")
		if grant == "otp" {
			_, _ = w.Write([]byte(`{"data":{"access_token":"ACCESS-TEST","refresh_token":"REFRESH-TEST","expires_at":"2099-01-01T00:00:00Z"}}`))
			return
		}
		if grant == "refresh_token" {
			_, _ = w.Write([]byte(`{"data":{"access_token":"ACCESS-BARU","refresh_token":"REFRESH-BARU","expires_at":"2099-01-01T00:00:00Z"}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	mux.HandleFunc("/goresto/v5/public/users/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ACCESS-TEST" {
			t.Errorf("config Authorization = %q, mau Bearer ACCESS-TEST", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"merchant":{"id":"MERCHANT-TEST-001","outlet_name":"Toko Tes"}}}`))
	})
	mux.HandleFunc("/merchant-analytics/v2/merchants/transactions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "access_token=ACCESS-TEST; refresh_token=REFRESH-TEST" {
			t.Errorf("Cookie = %q, mau access_token + refresh_token dari sesi", r.Header.Get("Cookie"))
		}
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		if size != 20 {
			t.Errorf("size = %d, mau 20", size)
		}
		// Total 25 transaksi: halaman 0 -> 20, halaman 20 -> 5, sisanya kosong.
		var items []map[string]any
		for i := from; i < from+size && i < 25; i++ {
			items = append(items, map[string]any{
				"transaction_id": fmt.Sprintf("TX-PAGE-%02d", i),
				"amount":         221600,
				"transacted_at":  "2026-10-28T10:10:00+07:00",
			})
		}
		if items == nil {
			items = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	})
	mux.HandleFunc("/transactions-401", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/transactions-429", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	return httptest.NewServer(mux)
}

func testConfig(serverURL string) Config {
	return Config{
		BaseURL: serverURL, AuthBaseURL: serverURL,
		MerchantID:       "MERCHANT-FALLBACK",
		SessionFile:      filepath.Join(os.TempDir(), "tidak-ada-sesi-gobiz-test.json"),
		TransactionsPath: "/merchant-analytics/v2/merchants/transactions",
		OtpPath:          "/goid/login/request", VerifyPath: "/goid/token",
		RefreshPath: "/goid/token", MerchantConfigPath: "/goresto/v5/public/users/config",
	}
}

func TestAuthFlowLengkap(t *testing.T) {
	srv := mockGobizServer(t)
	defer srv.Close()
	cfg := testConfig(srv.URL)
	ctx := context.Background()

	otp, err := cfg.RequestOtp(ctx, "08123456789")
	if err != nil {
		t.Fatalf("RequestOtp: %v", err)
	}
	if otp.OtpToken != "OTP-TOKEN-TEST" || otp.OtpLength != 4 || otp.OtpExpiresIn != 720 {
		t.Fatalf("OTP = %+v, mau token OTP-TOKEN-TEST panjang 4 expiry 720", otp)
	}
	session, err := cfg.VerifyOtp(ctx, otp.OtpToken, "1234")
	if err != nil {
		t.Fatalf("VerifyOtp: %v", err)
	}
	if session.AccessToken != "ACCESS-TEST" || session.RefreshToken != "REFRESH-TEST" {
		t.Fatalf("sesi = %+v", session)
	}
	merchant, err := cfg.FetchMerchantConfig(ctx, session)
	if err != nil {
		t.Fatalf("FetchMerchantConfig: %v", err)
	}
	if merchant.MerchantID != "MERCHANT-TEST-001" || merchant.OutletName != "Toko Tes" {
		t.Fatalf("merchant = %+v", merchant)
	}
	refreshed, err := cfg.RefreshSession(ctx, "REFRESH-TEST", "")
	if err != nil {
		t.Fatalf("RefreshSession: %v", err)
	}
	if refreshed.AccessToken != "ACCESS-BARU" {
		t.Fatalf("refresh = %+v, mau ACCESS-BARU", refreshed)
	}
}

func TestXUniqueidStabilPerProses(t *testing.T) {
	if ProcessUniqueID() != ProcessUniqueID() || ProcessUniqueID() == "" {
		t.Fatal("x-uniqueid harus stabil per proses dan tidak kosong")
	}
	if got := DashboardHeaders()["X-Uniqueid"]; got != ProcessUniqueID() {
		t.Fatalf("header X-Uniqueid = %q, mau sama dengan ID proses", got)
	}
}

func TestNormalizePhoneNational(t *testing.T) {
	cases := map[string]string{
		"08123456789":      "8123456789",
		"+62 812-3456-789": "8123456789",
		"628123456789":     "8123456789",
		"8123456789":       "8123456789",
	}
	for in, want := range cases {
		if got := NormalizePhoneNational(in); got != want {
			t.Fatalf("Normalize(%q) = %q, mau %q", in, got, want)
		}
	}
}

func TestPagination20Plus5(t *testing.T) {
	srv := mockGobizServer(t)
	defer srv.Close()
	cfg := testConfig(srv.URL)
	session := &Session{
		AccessToken: "ACCESS-TEST", RefreshToken: "REFRESH-TEST",
		ExpiresAt: "2099-01-01T00:00:00Z", MerchantID: "MERCHANT-TEST-001",
	}
	mutations, skipped, err := cfg.FetchTransactionsDetailed(context.Background(), FetchOptions{Session: session})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(mutations) != 25 {
		t.Fatalf("mutasi = %d, mau 25 (20 + 5, bukan hanya 20 pertama)", len(mutations))
	}
	if mutations[24].Amount != 2216 {
		t.Fatalf("mutasi terakhir amount = %d, mau 2216 (221600 minor / 100)", mutations[24].Amount)
	}
	if mutations[24].TransactionID != "TX-PAGE-24" {
		t.Fatalf("mutasi terakhir = %q, mau TX-PAGE-24", mutations[24].TransactionID)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, mau 0", skipped)
	}
}

func TestFetchTanpaSesiDanErrorHTTP(t *testing.T) {
	srv := mockGobizServer(t)
	defer srv.Close()
	cfg := testConfig(srv.URL)
	// Tanpa sesi di file dan tanpa sesi titipan -> sesi-tidak-valid.
	if _, _, err := cfg.FetchTransactionsDetailed(context.Background(), FetchOptions{}); err == nil {
		t.Fatal("fetch tanpa sesi harus error")
	} else {
		var ge *Error
		if !asGobizError(err, &ge) || ge.Code != CodeSessionInvalid {
			t.Fatalf("err = %v, mau sesi-tidak-valid", err)
		}
	}
	// 401 -> sesi-tidak-valid, 429 -> rate-limit.
	session := &Session{AccessToken: "X", RefreshToken: "Y", ExpiresAt: "2099-01-01T00:00:00Z", MerchantID: "M"}
	for path, want := range map[string]ErrorCode{"/transactions-401": CodeSessionInvalid, "/transactions-429": CodeRateLimit} {
		cfg2 := testConfig(srv.URL)
		cfg2.TransactionsPath = path
		if _, _, err := cfg2.FetchTransactionsDetailed(context.Background(), FetchOptions{Session: session}); err == nil {
			t.Fatalf("%s: harus error", path)
		} else {
			var ge *Error
			if !asGobizError(err, &ge) || ge.Code != want {
				t.Fatalf("%s: err = %v, mau %s", path, err, want)
			}
		}
	}
}

func asGobizError(err error, target **Error) bool {
	for err != nil {
		if ge, ok := err.(*Error); ok {
			*target = ge
			return true
		}
		// errors.As manual tanpa import errors di helper ini.
		break
	}
	return false
}

func TestMapGobizTransactionsVarianFieldAsli(t *testing.T) {
	// Varian dari laporan studi deobfuscasi: id / gross_amount /
	// transaction_time / transaction_status. Pemetaan lama membuang
	// semua item seperti ini (skipped naik, usulan 0).
	raw := map[string]any{"data": []any{
		map[string]any{"id": "TX-ASLI-001", "gross_amount": json.Number("278000"), "transaction_time": "2026-10-29T12:20:00+07:00", "transaction_status": "SETTLEMENT", "payment_type": "QRIS"},
		map[string]any{"transaction_id": "TX-KANONIS-002", "amount": json.Number("221600"), "transacted_at": "2026-10-28T10:10:00+07:00", "status": "CAPTURE", "payment_type": "GOPAY"},
	}}
	muts, skipped := MapGobizTransactions(raw)
	if skipped != 0 || len(muts) != 2 {
		t.Fatalf("varian field: mutasi=%d skipped=%d, mau 2 dan 0", len(muts), skipped)
	}
	if muts[0].TransactionID != "TX-ASLI-001" || muts[0].Amount != 2780 || muts[0].Status != "settlement" || muts[0].PaymentType != "QRIS" {
		t.Fatalf("mutasi varian = %+v, mau TX-ASLI-001 2780 settlement QRIS", muts[0])
	}
	if muts[1].TransactionID != "TX-KANONIS-002" || muts[1].Status != "capture" || muts[1].PaymentType != "GOPAY" {
		t.Fatalf("mutasi kanonis = %+v, mau TX-KANONIS-002 capture GOPAY", muts[1])
	}
}

func TestMapGobizNominalTidakHabisDibagi100Dilewati(t *testing.T) {
	muts, skipped := MapGobizTransactions(map[string]any{"data": []any{
		map[string]any{"transaction_id": "TX-ANEH", "amount": json.Number("278001"), "transacted_at": "2026-10-29T12:20:00+07:00"},
	}})
	if len(muts) != 0 || skipped != 1 {
		t.Fatalf("nominal aneh: mutasi=%d skipped=%d, mau 0 dan 1", len(muts), skipped)
	}
}

func TestMapTransactionsItemTidakValidDilewati(t *testing.T) {
	mutations, skipped := MapGobizTransactions(map[string]any{
		"data": []any{
			map[string]any{"transaction_id": "TX-OK", "amount": json.Number("221600"), "transacted_at": "2026-10-28T10:10:00+07:00"},
			map[string]any{"transaction_id": "", "amount": json.Number("100"), "transacted_at": "2026-10-28T10:10:00+07:00"},
			map[string]any{"transaction_id": "TX-STR", "amount": "2216", "transacted_at": "2026-10-28T10:10:00+07:00"},
			map[string]any{"transaction_id": "TX-NEG", "amount": json.Number("-5"), "transacted_at": "2026-10-28T10:10:00+07:00"},
			map[string]any{"transaction_id": "TX-DATE", "amount": json.Number("100"), "transacted_at": "bukan-waktu"},
		},
	})
	if len(mutations) != 1 || mutations[0].TransactionID != "TX-OK" {
		t.Fatalf("mutasi = %+v, mau hanya TX-OK", mutations)
	}
	if skipped != 4 {
		t.Fatalf("skipped = %d, mau 4", skipped)
	}
	if _, skipped := MapGobizTransactions(map[string]any{"tidak-dikenal": true}); skipped != 1 {
		t.Fatalf("wadah tidak dikenali: skipped = %d, mau 1", skipped)
	}
}

func TestSessionSaveLoadTanpaHPOTP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gobiz-session.json")
	session := &Session{
		AccessToken: "ACCESS-TEST", RefreshToken: "REFRESH-TEST",
		ExpiresAt: "2099-01-01T00:00:00Z", MerchantID: "MERCHANT-TEST-001", OutletName: "Toko Tes",
	}
	if err := SaveSession(path, session); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	loaded, err := LoadSession(path)
	if err != nil || loaded == nil {
		t.Fatalf("LoadSession: %v %+v", err, loaded)
	}
	if loaded.MerchantID != "MERCHANT-TEST-001" || loaded.OutletName != "Toko Tes" {
		t.Fatalf("loaded = %+v", loaded)
	}
	content, _ := os.ReadFile(path)
	for _, forbidden := range []string{"phone", "otp", "0812"} {
		if strings.Contains(strings.ToLower(string(content)), forbidden) {
			t.Fatalf("file sesi mengandung %q, tidak boleh", forbidden)
		}
	}
	if _, err := LoadSession(filepath.Join(dir, "tidak-ada.json")); err != nil {
		t.Fatalf("file hilang harus (nil, nil), dapat err %v", err)
	}
}

func TestResolveSource(t *testing.T) {
	if s, err := ResolveSource(""); err != nil || s != SourceMock {
		t.Fatalf("kosong = %v %v, mau mock", s, err)
	}
	if s, err := ResolveSource("gobiz"); err != nil || s != SourceGobiz {
		t.Fatalf("gobiz = %v %v", s, err)
	}
	if _, err := ResolveSource("aneh"); err == nil {
		t.Fatal("SOURCE aneh harus error")
	}
}
