// Package gobiz adalah klien GoBiz transparan — penulisan ulang login
// dari laporan studi deobfuscasi (bukan blob obfuscate, bukan salinan
// login.js / sessionManager.js repo sumber).
//
// PERINGATAN KERAS — BACA SEBELUM MEMAKAI MODUL INI:
// Auth di sini MENIRU DASHBOARD WEB GoBiz (client web Gojek sendiri +
// header Origin/Referer portal merchant), BUKAN API partner resmi yang
// diterbitkan untuk proyek ini. Gojek bisa mengubah client_id /
// x-appversion / header / path kapan saja tanpa pemberitahuan, dan
// login bisa berhenti jalan. Pemakaian agresif berisiko pembatasan
// akun. Modul ini HANYA boleh diuji dengan akun dummy dulu, dan
// TIDAK BOLEH dipakai ke akun merchant produksi tanpa keputusan
// terpisah pemilik akun + kunci kontrak.
//
// MODUL INI HANYA MELAKUKAN GET TRANSAKSI (read-only) + alur login
// OTP. Tidak ada transfer, tidak ada ubah saldo/status, tidak ada
// auto-credit, tidak ada akses database backend.
//
// KEAMANAN SESI (lebih ketat dari repo sumber studi):
//   - File sesi lokal service HANYA berisi: accessToken, refreshToken,
//     expiresAt, dan bila berhasil diambil: merchantId, outletName.
//   - TIDAK PERNAH nomor HP, TIDAK PERNAH OTP, di file sesi, log,
//     pesan error, .env, kode, atau tes. Nomor HP hanya hidup di
//     memori selama request OTP berjalan; OTP hanya hidup di memori
//     selama verifikasi OTP berjalan.
//   - Karena nomor HP tidak disimpan, refresh TANPA nomor HP adalah
//     penyimpangan sadar dari repo sumber (di sana refresh membawa
//     phone_number dari file sesi). Apakah server live menerima
//     refresh tanpa phone_number MASIH BELUM TERBUKTI LIVE dan wajib
//     divalidasi saat uji akun dummy.
//   - File sesi TIDAK BOLEH di-commit atau dikirim ke siapa pun;
//     token di dalamnya plaintext. Lindungi file itu dengan izin
//     file OS, jangan taruh di folder yang di-share / di-backup publik.
//
// ENDPOINT TRANSAKSI:
// GET {GOBIZ_BASE_URL}/merchant-analytics/v2/merchants/transactions
// dengan parameter merchant_ids, start_time, end_time,
// statuses=SETTLEMENT,CAPTURE, payment_types=QRIS,GOPAY.
// Bentuk response transaksi asli masih belum terverifikasi live;
// pemetaan di sini toleran terhadap empat bentuk wadah mock
// (array langsung, { data: [...] }, { transactions: [...] },
// { data: { transactions: [...] } }) — dan TIDAK SATU PUN dari
// keempatnya diklaim sebagai bentuk asli.
package gobiz

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopay-merchant-go/internal/types"
)

// ErrorCode membedakan kegagalan yang bisa ditindaklanjuti operator
// (sesi habis / dicabut -> login ulang) dari kegagalan sementara
// (rate-limit / jaringan -> backoff, lalu coba lagi).
type ErrorCode string

const (
	CodeSessionInvalid ErrorCode = "sesi-tidak-valid"
	CodeRateLimit      ErrorCode = "rate-limit"
	CodeGobizError     ErrorCode = "sumber-gobiz-error"
	CodeConfigInvalid  ErrorCode = "konfigurasi-tidak-valid"
)

// Error adalah error klien GoBiz. Error ini dilempar, bukan ditelan —
// pemanggil wajib menanganinya dan TIDAK BOLEH fallback diam-diam
// ke mock. Pesan error TIDAK PERNAH menyertakan HP / OTP / token.
// HTTPStatus adalah kode HTTP GoBiz bila ada (0 bila jaringan /
// parse gagal): angka ini AMAN dikembalikan ke UI sebagai diagnosis,
// karena tidak mengandung token, merchant ID, atau parameter URL.
type Error struct {
	Code       ErrorCode
	Msg        string
	HTTPStatus int
}

func (e *Error) Error() string { return e.Msg }

func gobizErr(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Session adalah sesi GoBiz yang disimpan di file lokal service.
// MerchantId / OutletName opsional: diisi dari langkah config login
// bila berhasil; bila tidak ada, pemakaian transaksi memakai
// GOBIZ_MERCHANT_ID dari env sebagai fallback.
type Session struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	MerchantID   string `json:"merchantId,omitempty"`
	OutletName   string `json:"outletName,omitempty"`
}

// MerchantConfig adalah hasil langkah config merchant (langkah 3 login).
type MerchantConfig struct {
	MerchantID string
	OutletName string
}

// OtpRequestResult adalah hasil langkah request OTP (langkah 1 login).
type OtpRequestResult struct {
	OtpToken     string
	OtpLength    int
	OtpExpiresIn int
}

// Config adalah konfigurasi klien GoBiz.
type Config struct {
	BaseURL            string
	AuthBaseURL        string
	MerchantID         string // Hanya fallback; merchant dari sesi diutamakan.
	SessionFile        string
	TransactionsPath   string
	OtpPath            string
	VerifyPath         string
	RefreshPath        string
	MerchantConfigPath string
	HTTPClient         *http.Client // Nil = default client dengan timeout.
}

const (
	DefaultBaseURL            = "https://api.gojekapi.com"
	DefaultAuthBaseURL        = "https://api.gobiz.co.id"
	DefaultTransactionsPath   = "/merchant-analytics/v2/merchants/transactions"
	DefaultOtpPath            = "/goid/login/request"
	DefaultVerifyPath         = "/goid/token"
	DefaultRefreshPath        = "/goid/token"
	DefaultMerchantConfigPath = "/goresto/v5/public/users/config"
	// ClientID adalah client dashboard web Gojek (dari laporan studi;
	// bukan secret).
	ClientID       = "go-biz-web-new"
	PortalOrigin   = "https://portal.gofoodmerchant.co.id"
	PortalReferer  = "https://portal.gofoodmerchant.co.id/"
	DashboardAppID = "go-biz-web-dashboard"
	// DashboardAppVersion dipatok di kode sumber studi — titik rapuh:
	// bila Gojek menaikkan versi dashboard, nilai ini bisa kedaluwarsa.
	DashboardAppVersion     = "platform-v3.111.0-1708bc9a"
	DashboardUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
	TransactionStatuses     = "SETTLEMENT,CAPTURE"
	TransactionPaymentTypes = "QRIS,GOPAY"
	// RefreshMargin adalah ambang refresh: sesi dianggap perlu
	// di-refresh bila sisa waktunya < 5 menit.
	RefreshMargin = 5 * time.Minute
	// SessionTTL adalah masa berlaku sesi bila respons token tidak
	// memberi expiry: 24 jam.
	SessionTTL    = 24 * time.Hour
	AuthTimeout   = 15 * time.Second
	ConfigTimeout = 10 * time.Second
	PageSize      = 20
	MaxPages      = 100
)

// processUniqueID adalah x-uniqueid STABIL PER PROSES (dari laporan
// studi: digenerate per proses, bukan per panggilan). Perbaikan
// 2026-10-28 setelah uji live v3 gagal 401 di verify padahal OTP
// cepat diketik: versi sebelumnya membuat UUID baru setiap panggilan,
// sehingga request OTP dan verify memakai UUID berbeda — kalau server
// mengikat otp_token ke UUID peminta, verify akan ditolak.
var processUniqueID = newUUID()

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ProcessUniqueID mengembalikan x-uniqueid proses ini (untuk tes).
func ProcessUniqueID() string { return processUniqueID }

// DashboardHeaders mengembalikan header dashboard web GoBiz.
func DashboardHeaders() map[string]string {
	return map[string]string{
		"Accept":              "application/json, text/plain, */*",
		"Accept-Language":     "id",
		"Authentication-Type": "go-id",
		"Content-Type":        "application/json",
		"Gojek-Country-Code":  "ID",
		"Gojek-Timezone":      "Asia/Jakarta",
		"Origin":              PortalOrigin,
		"Referer":             PortalReferer,
		"User-Agent":          DashboardUserAgent,
		"X-Appid":             DashboardAppID,
		"X-Appversion":        DashboardAppVersion,
		"X-Deviceos":          "Web",
		"X-Phonemake":         "Windows 10 64-bit",
		"X-Phonemodel":        "Chrome 150.0.0.0 on Windows 10 64-bit",
		"X-Platform":          "Web",
		"X-Uniqueid":          processUniqueID,
		"X-User-Locale":       "en-GB",
		"X-User-Type":         "merchant",
	}
}

func nonEmptyEnv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// ConfigFromEnv membangun konfigurasi dari env. Nama variabel
// (lihat .env.example): GOBIZ_BASE_URL, GOBIZ_AUTH_BASE_URL,
// GOBIZ_MERCHANT_ID, GOBIZ_SESSION_FILE, GOBIZ_TRANSACTIONS_PATH,
// GOBIZ_OTP_PATH, GOBIZ_VERIFY_PATH, GOBIZ_REFRESH_PATH,
// GOBIZ_MERCHANT_CONFIG_PATH. Semua nilai di .env.example adalah
// dummy; jangan taruh nilai asli di sana.
func ConfigFromEnv() Config {
	base := nonEmptyEnv("GOBIZ_BASE_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	auth := nonEmptyEnv("GOBIZ_AUTH_BASE_URL")
	if auth == "" {
		auth = DefaultAuthBaseURL
	}
	sessionRaw := nonEmptyEnv("GOBIZ_SESSION_FILE")
	sessionFile := sessionRaw
	if sessionFile == "" {
		sessionFile = ".gobiz-session.json"
	}
	if !filepath.IsAbs(sessionFile) {
		if wd, err := os.Getwd(); err == nil {
			sessionFile = filepath.Join(wd, sessionFile)
		}
	}
	pathOr := func(key, def string) string {
		if v := nonEmptyEnv(key); v != "" {
			return v
		}
		return def
	}
	return Config{
		BaseURL: base, AuthBaseURL: auth,
		MerchantID:         nonEmptyEnv("GOBIZ_MERCHANT_ID"),
		SessionFile:        sessionFile,
		TransactionsPath:   pathOr("GOBIZ_TRANSACTIONS_PATH", DefaultTransactionsPath),
		OtpPath:            pathOr("GOBIZ_OTP_PATH", DefaultOtpPath),
		VerifyPath:         pathOr("GOBIZ_VERIFY_PATH", DefaultVerifyPath),
		RefreshPath:        pathOr("GOBIZ_REFRESH_PATH", DefaultRefreshPath),
		MerchantConfigPath: pathOr("GOBIZ_MERCHANT_CONFIG_PATH", DefaultMerchantConfigPath),
	}
}

// Source adalah saklar sumber: "mock" (default) atau "gobiz".
type Source string

const (
	SourceMock  Source = "mock"
	SourceGobiz Source = "gobiz"
)

// ResolveSource menormalisasi saklar SOURCE. Nilai kosong / tidak
// diisi = "mock". Nilai selain "mock"/"gobiz" -> error konfigurasi
// tidak valid; pemanggil harus menjawab HTTP 500, bukan menebak
// sumber lain.
func ResolveSource(value string) (Source, error) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	switch normalized {
	case "", "mock":
		return SourceMock, nil
	case "gobiz":
		return SourceGobiz, nil
	default:
		return "", gobizErr(CodeConfigInvalid, "Nilai SOURCE tidak dikenal: %q. Yang diizinkan hanya \"mock\" atau \"gobiz\".", normalized)
	}
}

// NormalizePhoneNational menormalisasi nomor HP ke bentuk nasional
// (dari laporan studi): buang semua non-digit, lalu buang awalan
// "62" atau "0" sekali. Contoh: "0812..." -> "812...",
// "+62 812..." -> "812...", "812..." tetap "812...". Fungsi ini
// TIDAK menyimpan nomor ke mana pun.
func NormalizePhoneNational(phone string) string {
	var digits strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if strings.HasPrefix(d, "62") {
		return d[2:]
	}
	if strings.HasPrefix(d, "0") {
		return d[1:]
	}
	return d
}

func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func isNonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// --- Penyimpanan sesi (file lokal service saja) ---

func parseSession(raw any) (*Session, bool) {
	m, ok := asMap(raw)
	if !ok {
		return nil, false
	}
	access, ok := m["accessToken"].(string)
	if !ok || strings.TrimSpace(access) == "" {
		return nil, false
	}
	refresh, ok := m["refreshToken"].(string)
	if !ok || strings.TrimSpace(refresh) == "" {
		return nil, false
	}
	expires, ok := m["expiresAt"].(string)
	if !ok || strings.TrimSpace(expires) == "" {
		return nil, false
	}
	if _, err := time.Parse(time.RFC3339, expires); err != nil {
		if _, err2 := time.Parse(time.RFC3339Nano, expires); err2 != nil {
			return nil, false
		}
	}
	s := &Session{AccessToken: access, RefreshToken: refresh, ExpiresAt: expires}
	// Field opsional; field phone/OTP bila ada di file LAMA sengaja
	// TIDAK dibaca dan TIDAK dikembalikan.
	if v, ok := m["merchantId"].(string); ok && strings.TrimSpace(v) != "" {
		s.MerchantID = v
	}
	if v, ok := m["outletName"].(string); ok && strings.TrimSpace(v) != "" {
		s.OutletName = v
	}
	return s, true
}

// LoadSession memuat sesi dari file. File hilang -> (nil, nil)
// (pemanggil harus gagal jelas, bukan fallback ke mock). File ada
// tapi JSON rusak / field tidak lengkap -> error sesi-tidak-valid.
// TIDAK PERNAH mengembalikan sesi setengah jadi.
func LoadSession(sessionFile string) (*Session, error) {
	text, err := os.ReadFile(sessionFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, gobizErr(CodeSessionInvalid, "File sesi GoBiz tidak bisa dibaca (%s).", sessionFile)
	}
	var parsed any
	if err := json.Unmarshal(text, &parsed); err != nil {
		return nil, gobizErr(CodeSessionInvalid, "File sesi GoBiz bukan JSON yang valid (%s).", sessionFile)
	}
	s, ok := parseSession(parsed)
	if !ok {
		return nil, gobizErr(CodeSessionInvalid, "File sesi GoBiz tidak lengkap (butuh accessToken, refreshToken, expiresAt yang valid) (%s).", sessionFile)
	}
	return s, nil
}

// SaveSession menyimpan sesi ke file lokal. Hanya field sesi yang
// ditulis — TIDAK PERNAH nomor HP atau OTP.
func SaveSession(sessionFile string, session *Session) error {
	body, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return gobizErr(CodeGobizError, "Gagal menyimpan file sesi GoBiz (%s).", sessionFile)
	}
	if dir := filepath.Dir(sessionFile); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return gobizErr(CodeGobizError, "Gagal menyimpan file sesi GoBiz (%s).", sessionFile)
		}
	}
	if err := os.WriteFile(sessionFile, body, 0o600); err != nil {
		return gobizErr(CodeGobizError, "Gagal menyimpan file sesi GoBiz (%s).", sessionFile)
	}
	return nil
}

// IsSessionExpiredOrNear adalah true bila sesi sudah lewat /
// mendekati kedaluwarsa (margin 5 menit).
func IsSessionExpiredOrNear(session *Session, now time.Time, margin time.Duration) bool {
	exp, err := time.Parse(time.RFC3339Nano, session.ExpiresAt)
	if err != nil {
		return true
	}
	return exp.Sub(now) <= margin
}

// --- HTTP ---

// Satu panggilan HTTP dengan pemetaan error yang jujur:
// 401 -> sesi-tidak-valid, 429 -> rate-limit, status lain yang gagal
// / gagal jaringan / timeout -> sumber-gobiz-error.
// TIDAK PERNAH fallback ke mock, TIDAK PERNAH menelan error.
// Pesan error TIDAK PERNAH menyertakan nomor HP / OTP / token.
func (c Config) do(ctx context.Context, method, url string, headers map[string]string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, gobizErr(CodeGobizError, "Gagal membuat body request GoBiz.")
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, gobizErr(CodeGobizError, "Gagal membuat request GoBiz (%s).", url)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.client().Do(req)
	if err != nil {
		return nil, gobizErr(CodeGobizError, "Gagal menghubungi GoBiz (%s): %s.", url, err.Error())
	}
	if res.StatusCode == 401 {
		res.Body.Close()
		return nil, &Error{Code: CodeSessionInvalid, HTTPStatus: 401, Msg: fmt.Sprintf("GoBiz menjawab 401 untuk %s. Sesi tidak valid / dicabut; login ulang diperlukan (hanya saat pemilik akun hadir).", url)}
	}
	if res.StatusCode == 429 {
		res.Body.Close()
		return nil, &Error{Code: CodeRateLimit, HTTPStatus: 429, Msg: fmt.Sprintf("GoBiz menjawab 429 untuk %s. Kena rate-limit; tunda dengan backoff, jangan memanggil ulang seketika.", url)}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		return nil, &Error{Code: CodeGobizError, HTTPStatus: res.StatusCode, Msg: fmt.Sprintf("GoBiz menjawab HTTP %d untuk %s.", res.StatusCode, url)}
	}
	return res, nil
}

func readJSONBody(res *http.Response, url string) (any, error) {
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, gobizErr(CodeGobizError, "Respons GoBiz bukan JSON yang valid (%s).", url)
	}
	var parsed any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, gobizErr(CodeGobizError, "Respons GoBiz bukan JSON yang valid (%s).", url)
	}
	return parsed, nil
}

// Ambil kandidat objek dari wadah data / data.data / body langsung.
func tokenCandidates(raw any) []map[string]any {
	var out []map[string]any
	top, ok := asMap(raw)
	if !ok {
		return out
	}
	if data, ok := asMap(top["data"]); ok {
		if inner, ok := asMap(data["data"]); ok {
			out = append(out, inner)
		}
		out = append(out, data)
	}
	out = append(out, top)
	return out
}

// Ambil sesi dari body token. Toleran terhadap wadah data / data.data
// dan penamaan snake_case/camelCase. Bila respons tidak memberi
// expiresAt/expires_in, expiry dihitung +24 jam dari sekarang.
func parseTokenBody(raw any) (*Session, bool) {
	for _, o := range tokenCandidates(raw) {
		access, _ := firstString(o, "accessToken", "access_token")
		refresh, _ := firstString(o, "refreshToken", "refresh_token")
		if !isNonEmptyString(access) || !isNonEmptyString(refresh) {
			continue
		}
		if expires, ok := firstString(o, "expiresAt", "expires_at"); ok && isNonEmptyString(expires) {
			if _, err := time.Parse(time.RFC3339Nano, expires); err == nil {
				return &Session{AccessToken: access, RefreshToken: refresh, ExpiresAt: expires}, true
			}
		}
		if expiresIn, ok := firstNumber(o, "expiresIn", "expires_in"); ok && expiresIn > 0 {
			return &Session{
				AccessToken: access, RefreshToken: refresh,
				ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339),
			}, true
		}
		return &Session{
			AccessToken: access, RefreshToken: refresh,
			ExpiresAt: time.Now().Add(SessionTTL).UTC().Format(time.RFC3339),
		}, true
	}
	return nil, false
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s, true
		}
	}
	return "", false
}

func firstNumber(m map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n, true
			}
		case float64:
			return int64(v), true
		}
	}
	return 0, false
}

// Ambil hasil request OTP. Field yang dibaca (dari laporan studi):
// otp_token (wajib), otp_length (default 4), otp_expires_in
// (default 720 detik). PERHATIAN: field `expires_in` BUKAN sumber
// expiry — mock dengan expires_in saja tetap menghasilkan 720.
func parseOtpBody(raw any) (*OtpRequestResult, bool) {
	for _, o := range tokenCandidates(raw) {
		token, ok := firstString(o, "otpToken", "otp_token")
		if !ok {
			continue
		}
		length := int64(4)
		if n, ok := firstNumber(o, "otpLength", "otp_length"); ok && n > 0 {
			length = n
		}
		expires := int64(720)
		if n, ok := firstNumber(o, "otpExpiresIn", "otp_expires_in"); ok && n > 0 {
			expires = n
		}
		return &OtpRequestResult{OtpToken: token, OtpLength: int(length), OtpExpiresIn: int(expires)}, true
	}
	return nil, false
}

// Ambil merchant/outlet dari body config. Field yang dibaca:
// data.merchant.id -> merchantId, data.merchant.outlet_name ->
// outletName. Toleran terhadap wadah data / data.data.
func parseMerchantConfigBody(raw any) (*MerchantConfig, bool) {
	top, ok := asMap(raw)
	if !ok {
		return nil, false
	}
	var candidates []any
	if data, ok := asMap(top["data"]); ok {
		if inner, ok := asMap(data["data"]); ok {
			candidates = append(candidates, inner)
		}
		candidates = append(candidates, data)
	}
	candidates = append(candidates, top)
	for _, cand := range candidates {
		c, ok := asMap(cand)
		if !ok {
			continue
		}
		merchant, ok := asMap(c["merchant"])
		if !ok {
			continue
		}
		id, ok := firstString(merchant, "id")
		name, _ := firstString(merchant, "outlet_name", "outletName")
		if ok && isNonEmptyString(id) && isNonEmptyString(name) {
			return &MerchantConfig{MerchantID: id, OutletName: name}, true
		}
	}
	return nil, false
}

// --- Alur login transparan ---

// RequestOtp adalah langkah 1 login — minta OTP. Nomor HP
// dinormalisasi ke bentuk nasional di memori saja dan TIDAK PERNAH
// ditulis ke file sesi, log, atau pesan error.
func (c Config) RequestOtp(ctx context.Context, phone string) (*OtpRequestResult, error) {
	national := NormalizePhoneNational(phone)
	if national == "" {
		return nil, gobizErr(CodeConfigInvalid, "Nomor HP untuk request OTP tidak boleh kosong.")
	}
	url := joinURL(c.AuthBaseURL, c.OtpPath)
	ctx, cancel := context.WithTimeout(ctx, AuthTimeout)
	defer cancel()
	res, err := c.do(ctx, http.MethodPost, url, DashboardHeaders(), map[string]any{
		"client_id": ClientID, "phone_number": national, "country_code": "62",
	})
	if err != nil {
		return nil, err
	}
	parsed, err := readJSONBody(res, url)
	if err != nil {
		return nil, err
	}
	result, ok := parseOtpBody(parsed)
	if !ok {
		return nil, gobizErr(CodeGobizError, "Respons request OTP tidak berisi otp_token yang valid.")
	}
	return result, nil
}

// VerifyOtp adalah langkah 2 login — verifikasi OTP menjadi sesi.
// OTP TIDAK PERNAH disimpan — hanya token hasil verifikasi yang
// boleh disimpan (oleh pemanggil, via SaveSession) di file lokal.
func (c Config) VerifyOtp(ctx context.Context, otpToken, otpCode string) (*Session, error) {
	if !isNonEmptyString(otpToken) || !isNonEmptyString(otpCode) {
		return nil, gobizErr(CodeConfigInvalid, "otpToken dan kode OTP tidak boleh kosong.")
	}
	url := joinURL(c.AuthBaseURL, c.VerifyPath)
	ctx, cancel := context.WithTimeout(ctx, AuthTimeout)
	defer cancel()
	res, err := c.do(ctx, http.MethodPost, url, DashboardHeaders(), map[string]any{
		"client_id": ClientID, "grant_type": "otp",
		"data": map[string]any{"otp": otpCode, "otp_token": otpToken},
	})
	if err != nil {
		return nil, err
	}
	parsed, err := readJSONBody(res, url)
	if err != nil {
		return nil, err
	}
	session, ok := parseTokenBody(parsed)
	if !ok {
		return nil, gobizErr(CodeGobizError, "Respons verifikasi OTP tidak berisi token sesi yang valid.")
	}
	return session, nil
}

// FetchMerchantConfig adalah langkah 3 login — ambil merchant/outlet
// dari sesi yang sudah terverifikasi. Gagal TIDAK BOLEH membuat
// pemanggil menebak merchant_id; pemanggil memakai fallback env bila
// ada, atau gagal jelas saat polling.
func (c Config) FetchMerchantConfig(ctx context.Context, session *Session) (*MerchantConfig, error) {
	url := joinURL(c.AuthBaseURL, c.MerchantConfigPath)
	ctx, cancel := context.WithTimeout(ctx, ConfigTimeout)
	defer cancel()
	headers := map[string]string{
		"Accept":              "application/json, text/plain, */*",
		"Authentication-Type": "go-id",
		"Authorization":       "Bearer " + session.AccessToken,
		"Origin":              PortalOrigin,
		"Referer":             PortalReferer,
		"User-Agent":          DashboardUserAgent,
	}
	res, err := c.do(ctx, http.MethodGet, url, headers, nil)
	if err != nil {
		return nil, err
	}
	parsed, err := readJSONBody(res, url)
	if err != nil {
		return nil, err
	}
	merchant, ok := parseMerchantConfigBody(parsed)
	if !ok {
		return nil, gobizErr(CodeGobizError, "Respons config merchant tidak berisi merchant.id / outlet_name yang valid.")
	}
	return merchant, nil
}

// RefreshSession me-refresh sesi dengan refresh token. Body dari
// laporan studi: client_id, grant_type refresh_token, data
// { refresh_token, phone_number, country_code }.
//
// PENYIMPANGAN SADAR: karena nomor HP tidak disimpan di file sesi,
// parameter phone di sini OPSIONAL. Bila pemanggil punya nomor di
// memori, ia diteruskan (dinormalisasi, tidak disimpan); bila tidak,
// refresh dikirim tanpa phone_number. Apakah server live menerima
// refresh tanpa phone_number MASIH BELUM TERBUKTI LIVE.
func (c Config) RefreshSession(ctx context.Context, refreshToken, phone string) (*Session, error) {
	if !isNonEmptyString(refreshToken) {
		return nil, gobizErr(CodeSessionInvalid, "Refresh token kosong; sesi tidak bisa di-refresh, login ulang diperlukan.")
	}
	data := map[string]any{"refresh_token": refreshToken}
	if national := NormalizePhoneNational(phone); national != "" {
		data["phone_number"] = national
		data["country_code"] = "62"
	}
	url := joinURL(c.AuthBaseURL, c.RefreshPath)
	ctx, cancel := context.WithTimeout(ctx, ConfigTimeout)
	defer cancel()
	res, err := c.do(ctx, http.MethodPost, url, DashboardHeaders(), map[string]any{
		"client_id": ClientID, "grant_type": "refresh_token", "data": data,
	})
	if err != nil {
		return nil, err
	}
	parsed, err := readJSONBody(res, url)
	if err != nil {
		return nil, err
	}
	session, ok := parseTokenBody(parsed)
	if !ok {
		return nil, gobizErr(CodeGobizError, "Respons refresh tidak berisi token sesi yang valid.")
	}
	return session, nil
}

// --- Pemetaan response transaksi -> Mutasi internal (fungsi murni) ---

func extractTransactionItems(raw any) ([]any, bool) {
	if arr, ok := raw.([]any); ok {
		return arr, true
	}
	m, ok := asMap(raw)
	if !ok {
		return nil, false
	}
	if arr, ok := m["data"].([]any); ok {
		return arr, true
	}
	if arr, ok := m["transactions"].([]any); ok {
		return arr, true
	}
	if data, ok := asMap(m["data"]); ok {
		if arr, ok := data["transactions"].([]any); ok {
			return arr, true
		}
	}
	return nil, false
}

// MapGobizTransactions adalah pemetaan MURNI response GoBiz -> Mutasi.
//
// Aturan per item (item tidak valid dilewati, TIDAK PERNAH crash):
//   - transaction_id HARUS string non-kosong, kalau tidak -> dilewati.
//   - amount HARUS number integer >= 0. String angka, float, NaN,
//     negatif, boolean, atau hilang -> DITOLAK per item.
//   - transacted_at HARUS string tanggal yang valid, kalau tidak -> dilewati.
//   - Field tak dikenal diabaikan; source output selalu "gobiz".
//
// `skipped` menghitung item yang dilewati. Payload yang wadahnya
// tidak dikenali sama sekali dihitung sebagai 1 skipped (bukan 0),
// agar perubahan bentuk API yang tidak resmi itu tidak hilang
// diam-diam sebagai "tidak ada mutasi".
func MapGobizTransactions(raw any) (mutations []types.Mutasi, skipped int) {
	items, ok := extractTransactionItems(raw)
	if !ok {
		return nil, 1
	}
	for _, item := range items {
		rec, ok := asMap(item)
		if !ok {
			skipped++
			continue
		}
		// Nama field GoBiz asli TIDAK PERNAH terverifikasi live penuh:
		// laporan studi deobfuscasi menunjukkan varian id /
		// gross_amount / real_gross_amount / transaction_time /
		// settlement_time / transaction_status, sedangkan pemetaan
		// lama hanya menerima transaction_id / amount /
		// transacted_at. Bila varian itu yang dikirim GoBiz, pemetaan
		// lama akan membuang SEMUA mutasi asli diam-diam (skipped
		// naik, usulan 0) — kandidat akar masalah "sudah bayar masih
		// PENDING" yang belum pernah diuji live dengan mutasi asli
		// di dalam jendela. Jadi terima varian itu, dengan urutan
		// prioritas: nama kanonis dulu, lalu varian studi. Yang
		// tidak pernah dilakukan: memakai order_id sebagai
		// transaction_id (itu pengenal lain, bukan ID transaksi).
		txID, _ := firstString(rec, "transaction_id", "id")
		if strings.TrimSpace(txID) == "" {
			skipped++
			continue
		}
		amount, ok := parseAmount(rec["amount"])
		if !ok {
			amount, ok = parseAmount(rec["gross_amount"])
		}
		if !ok {
			amount, ok = parseAmount(rec["real_gross_amount"])
		}
		if !ok {
			skipped++
			continue
		}
		// BUKTI LIVE 2026-10-29 (bukan
		// tebakan): pembayaran Rp 2.780 tampil 278000 dan Rp 2.216
		// tampil 221600, dengan waktu yang persis cocok (12:20 dan
		// 17:28). Jadi GoBiz mengirim nominal dalam satuan minor
		// (sen) = rupiah x 100, BUKAN rupiah langsung. Pemetaan
		// lama memakai angka itu mentah-mentah, sehingga wire
		// membandingkan 278000 dengan total order 2780 dan TIDAK
		// PERNAH cocok — ini akar masalah "sudah bayar masih
		// PENDING", bukan sekadar tampilan kelebihan nol. Konversi
		// di sini, sekali, supaya matching, proposal, audit,
		// wire-log, dan /mutations semuanya memakai rupiah.
		// Bila tidak habis dibagi 100, item dilewati: itu bukan
		// satuan minor rupiah yang utuh, dan menebak pembulatannya
		// akan merusak matching nominal persis.
		if amount%100 != 0 {
			skipped++
			continue
		}
		amount /= 100
		transacted, _ := firstString(rec, "transacted_at", "transaction_time", "settlement_time")
		if strings.TrimSpace(transacted) == "" {
			skipped++
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, transacted); err != nil {
			if _, err2 := time.Parse(time.RFC3339, transacted); err2 != nil {
				skipped++
				continue
			}
		}
		// Elemen trace apa adanya, tidak ditebak: kosong bila tak dikirim.
		status, _ := firstString(rec, "status", "transaction_status")
		paymentType, _ := firstString(rec, "payment_type", "payment_method")
		mutations = append(mutations, types.Mutasi{
			TransactionID: txID, Amount: amount, TransactedAt: transacted, Source: "gobiz",
			Status: strings.ToLower(strings.TrimSpace(status)), PaymentType: paymentType,
		})
	}
	return mutations, skipped
}

func parseAmount(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil && i >= 0
	case float64:
		if n != float64(int64(n)) || n < 0 {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

// --- Ambil transaksi (GET saja, memakai sesi dari file) ---

// FetchOptions adalah opsi pengambilan transaksi.
type FetchOptions struct {
	StartTime string
	EndTime   string
	// Session yang sudah dipegang pemanggil. Bila diisi, file sesi
	// TIDAK dibaca dan TIDAK ditulis (berguna untuk tes); bila tidak
	// diisi, sesi dibaca dari file dan hasil refresh disimpan kembali.
	Session *Session
	// MaxPages membatasi halaman pagination (default 100 x 20).
	MaxPages int
}

func (c Config) resolveSessionForFetch(ctx context.Context, opts FetchOptions) (*Session, error) {
	if opts.Session != nil {
		if IsSessionExpiredOrNear(opts.Session, time.Now(), RefreshMargin) {
			return c.RefreshSession(ctx, opts.Session.RefreshToken, "")
		}
		return opts.Session, nil
	}
	stored, err := LoadSession(c.SessionFile)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, gobizErr(CodeSessionInvalid, "File sesi GoBiz tidak ditemukan (%s). Login OTP belum dilakukan; login live hanya saat pemilik akun hadir memberi OTP. Tidak ada fallback ke mock.", c.SessionFile)
	}
	if IsSessionExpiredOrNear(stored, time.Now(), RefreshMargin) {
		refreshed, err := c.RefreshSession(ctx, stored.RefreshToken, "")
		if err != nil {
			return nil, err
		}
		if refreshed.MerchantID == "" {
			refreshed.MerchantID = stored.MerchantID
		}
		if refreshed.OutletName == "" {
			refreshed.OutletName = stored.OutletName
		}
		if err := SaveSession(c.SessionFile, refreshed); err != nil {
			return nil, err
		}
		return refreshed, nil
	}
	return stored, nil
}

// FetchTransactionsDetailed mengambil transaksi dari GoBiz (GET saja)
// dan memetakannya ke Mutasi internal. Merchant yang dipakai:
// merchantId dari sesi (hasil langkah config login) BILA ADA, kalau
// tidak GOBIZ_MERCHANT_ID dari env sebagai fallback. Bila keduanya
// kosong -> konfigurasi-tidak-valid, dan mock TIDAK dipanggil sama
// sekali. Error yang dilempar: sesi-tidak-valid, rate-limit,
// sumber-gobiz-error. TIDAK ADA fallback ke mock.
//
// Perbaikan 2026-10-28 setelah /proposals live menjawab 422: bentuk
// request lama (tanpa from/size, tanpa Cookie) ditolak validasi
// merchant-analytics. Bentuk repo yang terbaca mengirim from=0,
// size=20, dan header Cookie dari sesi. Cookie dibangun saat fetch
// dari token yang sudah ada (tidak disimpan terpisah, tidak berisi
// nomor HP): access_token=...; refresh_token=...
//
// PAGINATION: halaman 20 transaksi diambil berulang (from = 0, 20,
// 40, ...) sampai satu halaman mengembalikan kurang dari 20 item,
// atau satu halaman tidak menambah transaction_id baru (pengaman
// untuk server mock yang mengembalikan halaman yang sama), atau
// batas MaxPages tercapai. Aturan matching TIDAK berubah.
func (c Config) FetchTransactionsDetailed(ctx context.Context, opts FetchOptions) (mutations []types.Mutasi, skipped int, err error) {
	session, err := c.resolveSessionForFetch(ctx, opts)
	if err != nil {
		return nil, 0, err
	}
	effectiveMerchantID := session.MerchantID
	if effectiveMerchantID == "" {
		effectiveMerchantID = c.MerchantID
	}
	if strings.TrimSpace(effectiveMerchantID) == "" {
		return nil, 0, gobizErr(CodeConfigInvalid, "Merchant ID tidak ada di sesi maupun di GOBIZ_MERCHANT_ID. Login harus berhasil mengambil config merchant, atau isi GOBIZ_MERCHANT_ID sebagai fallback.")
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = MaxPages
	}
	seen := make(map[string]bool)
	var all []types.Mutasi
	skippedTotal := 0
	for page := 0; page < maxPages; page++ {
		params := url.Values{}
		params.Set("merchant_ids", effectiveMerchantID)
		params.Set("statuses", TransactionStatuses)
		params.Set("payment_types", TransactionPaymentTypes)
		params.Set("from", fmt.Sprintf("%d", page*PageSize))
		params.Set("size", fmt.Sprintf("%d", PageSize))
		if opts.StartTime != "" {
			params.Set("start_time", opts.StartTime)
		}
		if opts.EndTime != "" {
			params.Set("end_time", opts.EndTime)
		}
		fullURL := joinURL(c.BaseURL, c.TransactionsPath) + "?" + params.Encode()
		headers := map[string]string{
			"Accept":              "application/json, text/plain, */*",
			"Authentication-Type": "go-id",
			"Authorization":       "Bearer " + session.AccessToken,
			"Cookie":              "access_token=" + session.AccessToken + "; refresh_token=" + session.RefreshToken,
			"Origin":              PortalOrigin,
			"Referer":             PortalReferer,
			"User-Agent":          DashboardUserAgent,
		}
		res, err := c.do(ctx, http.MethodGet, fullURL, headers, nil)
		if err != nil {
			return nil, 0, err
		}
		parsed, err := readJSONBody(res, fullURL)
		if err != nil {
			return nil, 0, err
		}
		pageMutations, pageSkipped := MapGobizTransactions(parsed)
		skippedTotal += pageSkipped
		newCount := 0
		for _, m := range pageMutations {
			if seen[m.TransactionID] {
				continue
			}
			seen[m.TransactionID] = true
			all = append(all, m)
			newCount++
		}
		if len(pageMutations) < PageSize || newCount == 0 {
			break
		}
	}
	return all, skippedTotal, nil
}

// FetchTransactions sama seperti FetchTransactionsDetailed, hanya
// mengembalikan mutasi.
func (c Config) FetchTransactions(ctx context.Context, opts FetchOptions) ([]types.Mutasi, error) {
	mutations, _, err := c.FetchTransactionsDetailed(ctx, opts)
	return mutations, err
}
