// Package server adalah HTTP server poller GoBiz (Fiber v3).
//
// Endpoint (SEMUA read-only terhadap saldo/status order — tidak ada
// endpoint yang mengubah saldo atau status order di mana pun):
//   - GET /health     : selalu merespons, tanpa membaca file data.
//   - GET /proposals  : menjalankan SATU siklus pencocokan dan
//     mengembalikan proposals + decisions siklus ini + log audit.
//     Sumber siklus dipilih saklar SOURCE: "mock" (default) memakai
//     file input dummy (data/orders.dummy.json +
//     data/mutations.dummy.json); "gobiz" memakai transaksi GoBiz
//     (orders tetap dari file dummy). File sesi hilang/tidak valid
//     -> HTTP 500 sumber-gobiz-error, TANPA fallback ke mock.
//     Nilai SOURCE lain -> HTTP 500 konfigurasi-tidak-valid.
//     CATATAN: endpoint ini memajukan state klaim LOKAL poller
//     (transaksi yang sudah diusulkan tidak diusulkan lagi pada
//     pemanggilan berikutnya). Restart service mengosongkan klaim
//     memori; bila store Postgres dipakai, klaim dimuat dari DB saat
//     start dan disimpan ke DB setiap siklus.
//   - GET /audit      : log audit lokal + daftar transaction_id yang
//     sudah diklaim secara lokal.
//   - GET /mutations  : RIWAYAT PEMBAYARAN, BACA SAJA. Mengembalikan
//     semua mutasi dalam rentang waktu (query start_time/end_time
//     RFC3339, opsional; di mode gobiz, kosong = 24 jam terakhir,
//     karena GoBiz menolak query tanpa rentang — terbukti live
//     2026-10-29), diurutkan terbaru dulu, TANPA memajukan
//     klaim, TANPA mengkredit, TANPA mengubah status order.
//     Sumber sama seperti /proposals (mock = file dummy, gobiz =
//     GoBiz live, tanpa fallback). Gunanya diagnostik: melihat apakah
//     sebuah pembayaran sudah tercatat di GoBiz, walau tidak cocok
//     dengan order mana pun. Hanya field aman yang dikembalikan
//     (transaction_id, amount, transacted_at, source); detail error
//     mentah sengaja tidak dikembalikan agar tidak membocorkan
//     parameter request internal.
//   - GET /           : halaman statis web/ui.html (dibaca dari disk
//     tiap request, seperti TS), tanpa CDN eksternal.
//   - GET /ui/config  : HANYA nilai aman. Sengaja tidak ada: token
//     internal, token sesi, nomor HP, OTP, merchant ID, QRIS payload,
//     isi .env, atau path file sesi.
//   - POST /ui/reserve : untuk UJI di localhost saja. Memanggil
//     reserveTotal() langsung di server, supaya token internal
//     /mdr/reserve TIDAK PERNAH keluar ke browser. Reservasi saja,
//     bukan kredit dan bukan order nyata.
//   - POST /mdr/reserve : jembatan POC, di balik token internal
//     POLLER_INTERNAL_TOKEN. Token kosong -> 500
//     konfigurasi-tidak-valid; token salah -> 401 unauthorized.
//   - POST /notify-order : push order baru dari backend (2026-10-29).
//     Mencatat order + membangunkan wire loop internal (fallback:
//     satu siklus wire lewat subprocess bila loop internal mati).
//     GET /notified-orders : daftar order yang masuk lewat push.
//
// Yang SENGAJA tidak ada: endpoint POST/PUT/DELETE lain, endpoint
// kredit saldo, endpoint ubah status, akses database mitra-tools,
// login OTP GoBiz, dan pemanggilan API GoBiz asli di luar transaksi.
//
// Server ini TIDAK mengkredit sendiri dan TIDAK mengubah status
// order sendiri. Settlement dilakukan wire loop — sejak 2026-10-29
// wire loop itu JALAN DI DALAM proses server ini (lihat cmd/server),
// bukan proses terpisah. POST /notify-order hanya MEMBANGUNKAN
// loop internal itu (bila sedang jalan); bila loop internal tidak
// jalan, fallback-nya tetap memicu satu siklus wire lewat subprocess
// gopay-merchant-go-wire.exe bila binary-nya ada. Bila keduanya tidak
// ada, notifikasi tetap dicatat dan tidak ada settlement sampai
// loop dinyalakan.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopay-merchant-go/internal/config"
	"gopay-merchant-go/internal/gobiz"
	"gopay-merchant-go/internal/match"
	"gopay-merchant-go/internal/mdr"
	"gopay-merchant-go/internal/mock"
	"gopay-merchant-go/internal/reserve"
	"gopay-merchant-go/internal/store"
	"gopay-merchant-go/internal/types"
)

const (
	ServiceName = "gopay-merchant-go-poller"
	Version     = "1.0.0"
	AuditCap    = 1000
)

// NotifiedOrder adalah order yang diberitahukan backend lewat push
// POST /notify-order (perintah Hanif 2026-10-29). Hanya field aman;
// tidak ada token, tidak ada data pembayaran mentah.
type NotifiedOrder struct {
	OrderID      string `json:"order_id"`
	Total        int64  `json:"total"`
	Price        int64  `json:"price"`
	Mdr          int64  `json:"mdr"`
	Fee          int64  `json:"fee"`
	CreditAmount int64  `json:"credit_amount"`
	ExpiresAt    string `json:"expires_at"`
	CreatedAt    string `json:"created_at"`
	ReceivedAt   string `json:"received_at"`
}

// Server adalah state server poller.
type Server struct {
	cfg     config.ServerConfig
	app     *fiber.App
	mu      sync.Mutex
	claimed map[string]bool
	audit   []types.AuditLogEntry
	store   store.Store
	// notified: order yang masuk lewat push, kunci = order_id.
	// Idempotent: push ulang order yang sama menimpa entri lama.
	notified map[string]NotifiedOrder
	// wireRunning: guard supaya notify tidak memicu dua siklus
	// wire bersamaan lewat subprocess (jalur fallback). Loop wire
	// internal (proses yang sama) tidak memakai guard ini.
	wireRunning bool
	// wireWake: channel bangun loop wire internal, disetel cmd/server
	// bila loop internal jalan. Nil = loop internal tidak jalan.
	wireWake chan struct{}
}

// SetWireWake memasang channel bangun untuk loop wire internal.
// Dipanggil cmd/server sebelum Listen; handler notify hanya jalan
// setelah Listen, jadi tidak ada balapan dengan pemasangan ini.
func (s *Server) SetWireWake(ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wireWake = ch
}

// NewServer membuat server dengan store yang sudah dibuka pemanggil
// (tes memakai MemoryStore; produksi memakai PostgresStore dari
// OpenStore). Server tidak menutup store sendiri; pemanggil yang
// menutupnya (lihat NewServerFromConfig).
func NewServer(cfg config.ServerConfig, st store.Store) *Server {
	s := &Server{cfg: cfg, claimed: make(map[string]bool), store: st, notified: make(map[string]NotifiedOrder)}
	if st != nil {
		if ids, err := st.ListClaimed(); err == nil {
			for _, id := range ids {
				s.claimed[id] = true
			}
		}
	}
	app := fiber.New(fiber.Config{AppName: ServiceName})
	app.Get("/health", s.handleHealth)
	app.Get("/proposals", s.handleProposals)
	app.Get("/audit", s.handleAudit)
	app.Get("/mutations", s.handleMutations)
	app.Get("/", s.handleUI)
	app.Get("/ui/config", s.handleUIConfig)
	app.Post("/ui/reserve", s.handleReserve(false))
	app.Post("/mdr/reserve", s.handleReserve(true))
	app.Post("/notify-order", s.handleNotifyOrder)
	app.Get("/notified-orders", s.handleNotifiedOrders)
	app.Use(func(c fiber.Ctx) error {
		return c.Status(404).JSON(fiber.Map{
			"error": "not-found",
			"note":  "Endpoint yang tersedia: GET / (UI), GET /health, GET /proposals, GET /audit, GET /mutations, GET /ui/config, GET /notified-orders, POST /ui/reserve, POST /mdr/reserve, POST /notify-order.",
		})
	})
	s.app = app
	return s
}

// App mengembalikan *fiber.App (untuk tes dan untuk Listen di main).
func (s *Server) App() *fiber.App { return s.app }

// OpenStore membuka store dari konfigurasi: Postgres (AutoMigrate)
// bila DATABASE_URL diisi, bila tidak MemoryStore untuk demo/tes.
// PERINGATAN: mode MemoryStore kehilangan klaim/reservasi saat
// restart — produksi WAJIB DATABASE_URL (lihat README).
func OpenStore(cfg config.ServerConfig) (store.Store, error) {
	if cfg.DatabaseURL != "" {
		return store.OpenPostgres(cfg.DatabaseURL)
	}
	return store.NewMemoryStore(), nil
}

// NewServerFromConfig membuat server sekaligus membuka store-nya.
// Mengembalikan server, fungsi penutup store, dan error.
func NewServerFromConfig(cfg config.ServerConfig) (*Server, func() error, error) {
	st, err := OpenStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	return NewServer(cfg, st), st.Close, nil
}

func (s *Server) pushAudit(entries []types.AuditLogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, entries...)
	for len(s.audit) > AuditCap {
		s.audit = s.audit[1:]
	}
}

func (s *Server) persistClaims(proposals []types.Proposal) {
	if s.store == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, p := range proposals {
		_ = s.store.InsertClaim(p.TransactionID, p.OrderID, now)
	}
}

type cycleOutcome struct {
	hadError bool
	payload  fiber.Map
}

func auditEntryNow(reason types.Reason) types.AuditLogEntry {
	return types.AuditLogEntry{
		LoggedAt: time.Now().UTC().Format(time.RFC3339),
		Decision: types.DecisionError, Reason: reason,
	}
}

// runFileCycle adalah satu siklus dari file dummy.
func (s *Server) runFileCycle() cycleOutcome {
	ordersRaw, err := mock.LoadOrdersRaw(s.cfg.DataDir)
	if err != nil {
		entry := auditEntryNow(types.ReasonMockError)
		s.pushAudit([]types.AuditLogEntry{entry})
		return cycleOutcome{hadError: true, payload: fiber.Map{
			"error": "sumber-mock-error", "detail": err.Error(),
			"data_dir": s.cfg.DataDir, "decisions": []types.AuditLogEntry{entry},
		}}
	}
	mutationsRaw, err := mock.LoadMutationsRaw(s.cfg.DataDir)
	if err != nil {
		entry := auditEntryNow(types.ReasonMockError)
		s.pushAudit([]types.AuditLogEntry{entry})
		return cycleOutcome{hadError: true, payload: fiber.Map{
			"error": "sumber-mock-error", "detail": err.Error(),
			"data_dir": s.cfg.DataDir, "decisions": []types.AuditLogEntry{entry},
		}}
	}
	s.mu.Lock()
	claimedCopy := make(map[string]bool, len(s.claimed))
	for k, v := range s.claimed {
		claimedCopy[k] = v
	}
	s.mu.Unlock()
	result := match.MatchRaw(ordersRaw, mutationsRaw, claimedCopy, match.MatchOptions{})
	s.mu.Lock()
	s.claimed = result.Claimed
	s.mu.Unlock()
	s.persistClaims(result.Proposals)
	s.pushAudit(result.Decisions)
	return cycleOutcome{hadError: false, payload: fiber.Map{
		"source": "mock", "proposals": result.Proposals,
		"decisions": result.Decisions, "audit_log": s.auditSnapshot(),
		"skipped_polling": result.SkippedPolling,
		"claimed":         s.claimedList(), "claimed_count": len(s.claimedList()),
	}}
}

// runGobizCycle adalah satu siklus sumber GoBiz. Orders tetap dibaca
// dari file dummy; mutasi dari GoBiz; TIDAK ADA fallback ke mock.
func (s *Server) runGobizCycle(ctx context.Context) cycleOutcome {
	ordersRaw, err := mock.LoadOrdersRaw(s.cfg.DataDir)
	if err != nil {
		entry := auditEntryNow(types.ReasonMockError)
		s.pushAudit([]types.AuditLogEntry{entry})
		return cycleOutcome{hadError: true, payload: fiber.Map{
			"source": "gobiz", "error": "sumber-mock-error", "detail": err.Error(),
			"data_dir": s.cfg.DataDir, "decisions": []types.AuditLogEntry{entry},
		}}
	}
	hasPending := false
	for _, raw := range ordersRaw {
		if m, ok := raw.(map[string]any); ok {
			if status, _ := m["status"].(string); status == "PENDING" {
				hasPending = true
				break
			}
		}
	}
	if !hasPending {
		s.mu.Lock()
		claimedCopy := make(map[string]bool, len(s.claimed))
		for k, v := range s.claimed {
			claimedCopy[k] = v
		}
		s.mu.Unlock()
		result := match.MatchRaw(ordersRaw, []any{}, claimedCopy, match.MatchOptions{})
		s.pushAudit(result.Decisions)
		return cycleOutcome{hadError: false, payload: fiber.Map{
			"source": "gobiz", "proposals": result.Proposals,
			"decisions": result.Decisions, "audit_log": s.auditSnapshot(),
			"skipped_polling": true, "claimed": s.claimedList(),
			"claimed_count": len(s.claimedList()),
			"gobiz_skipped": 0,
			"note":          "Tidak ada order PENDING; GoBiz tidak dipanggil pada siklus ini.",
		}}
	}
	// Jendela transaksi dari order PENDING: created_at paling awal
	// sampai grace_end_at paling akhir (yang masih bisa diparse).
	var startTime, endTime string
	for _, raw := range ordersRaw {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if status, _ := m["status"].(string); status != "PENDING" {
			continue
		}
		if created, _ := m["created_at"].(string); created != "" {
			if startTime == "" || created < startTime {
				startTime = created
			}
		}
		if grace, _ := m["grace_end_at"].(string); grace != "" {
			if endTime == "" || grace > endTime {
				endTime = grace
			}
		}
	}
	mutations, skipped, err := s.cfg.Gobiz.FetchTransactionsDetailed(ctx, gobiz.FetchOptions{
		StartTime: startTime, EndTime: endTime,
	})
	if err != nil {
		entry := auditEntryNow(types.ReasonGobizError)
		s.pushAudit([]types.AuditLogEntry{entry})
		code := "sumber-gobiz-error"
		var ge *gobiz.Error
		if errors.As(err, &ge) {
			code = string(ge.Code)
		}
		return cycleOutcome{hadError: true, payload: fiber.Map{
			"source": "gobiz", "error": "sumber-gobiz-error",
			"gobiz_code": code, "detail": err.Error(),
			"decisions": []types.AuditLogEntry{entry},
		}}
	}
	s.mu.Lock()
	claimedCopy := make(map[string]bool, len(s.claimed))
	for k, v := range s.claimed {
		claimedCopy[k] = v
	}
	s.mu.Unlock()
	// Siklus GoBiz menilai order dummy melawan mutasi GoBiz lewat
	// MatchRaw dengan mutasi yang sudah dipetakan (bukan mentah).
	mutationsRaw := make([]any, 0, len(mutations))
	for _, mu := range mutations {
		mutationsRaw = append(mutationsRaw, map[string]any{
			"transaction_id": mu.TransactionID, "amount": mu.Amount,
			"transacted_at": mu.TransactedAt, "source": mu.Source,
		})
	}
	result2 := match.MatchRaw(ordersRaw, mutationsRaw, claimedCopy, match.MatchOptions{})
	s.mu.Lock()
	s.claimed = result2.Claimed
	s.mu.Unlock()
	s.persistClaims(result2.Proposals)
	s.pushAudit(result2.Decisions)
	return cycleOutcome{hadError: false, payload: fiber.Map{
		"source": "gobiz", "proposals": result2.Proposals,
		"decisions": result2.Decisions, "audit_log": s.auditSnapshot(),
		"skipped_polling": result2.SkippedPolling,
		"claimed":         s.claimedList(), "claimed_count": len(s.claimedList()),
		"gobiz_skipped": skipped,
	}}
}

func (s *Server) auditSnapshot() []types.AuditLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]types.AuditLogEntry{}, s.audit...)
}

func (s *Server) claimedList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.claimed))
	for id := range s.claimed {
		out = append(out, id)
	}
	return out
}

// RunCycle adalah dispatcher satu siklus berdasarkan saklar SOURCE.
// SOURCE tidak valid -> payload error konfigurasi (bukan tebakan
// sumber lain, bukan fallback ke mock).
func (s *Server) RunCycle(ctx context.Context) cycleOutcome {
	source, err := gobiz.ResolveSource(s.cfg.Source)
	if err != nil {
		return cycleOutcome{hadError: true, payload: fiber.Map{
			"error": "konfigurasi-tidak-valid", "detail": err.Error(),
		}}
	}
	if source == gobiz.SourceGobiz {
		return s.runGobizCycle(ctx)
	}
	return s.runFileCycle()
}

func (s *Server) handleHealth(c fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"status": "ok", "service": ServiceName, "version": Version,
		"port": s.cfg.Port, "claimed_count": len(s.claimedList()),
		"audit_count": len(s.auditSnapshot()),
		"note":        "Poller hanya mengusulkan; tidak mengubah saldo/status order.",
	})
}

func (s *Server) handleProposals(c fiber.Ctx) error {
	outcome := s.RunCycle(c.Context())
	if outcome.hadError {
		return c.Status(500).JSON(outcome.payload)
	}
	return c.JSON(outcome.payload)
}

func (s *Server) handleAudit(c fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"audit_log": s.auditSnapshot(), "claimed": s.claimedList(),
	})
}

// parseWaktuMutasi mem-parse waktu RFC3339; string kosong -> waktu nol, tanpa error.
func parseWaktuMutasi(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, v)
}

// mutasiDariRaw mengubah satu item mutasi dummy (map) menjadi types.Mutasi.
// Amount bisa json.Number (UseNumber), float64, atau string.
func mutasiDariRaw(raw any) (types.Mutasi, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return types.Mutasi{}, false
	}
	txID, _ := m["transaction_id"].(string)
	if txID == "" {
		return types.Mutasi{}, false
	}
	var amount int64
	switch v := m["amount"].(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return types.Mutasi{}, false
		}
		amount = n
	case float64:
		amount = int64(v)
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return types.Mutasi{}, false
		}
		amount = n
	default:
		return types.Mutasi{}, false
	}
	transacted, _ := m["transacted_at"].(string)
	source, _ := m["source"].(string)
	return types.Mutasi{TransactionID: txID, Amount: amount, TransactedAt: transacted, Source: source}, true
}

// handleMutations adalah GET /mutations: riwayat pembayaran, baca saja.
// Tidak memajukan klaim, tidak menulis audit, tidak mengkredit.
// Query: start_time, end_time (RFC3339, opsional). Di mode gobiz,
// rentang diteruskan ke GoBiz; di mode mock, rentang difilter lokal.
func (s *Server) handleMutations(c fiber.Ctx) error {
	startRaw := c.Query("start_time")
	endRaw := c.Query("end_time")
	start, err := parseWaktuMutasi(startRaw)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "waktu-tidak-valid", "field": "start_time"})
	}
	end, err := parseWaktuMutasi(endRaw)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "waktu-tidak-valid", "field": "end_time"})
	}
	source, err := gobiz.ResolveSource(s.cfg.Source)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "konfigurasi-tidak-valid"})
	}
	// Perbaikan 2026-10-29, dari bukti live Hanif: /proposals (yang
	// selalu membawa rentang dari order dummy) lolos dengan sumber
	// gobiz dan Usulan 0, sedangkan /mutations tanpa rentang gagal
	// sumber-gobiz-error. Kesimpulan: GoBiz menolak query tanpa
	// rentang waktu. Jadi di mode gobiz, rentang kosong diisi default
	// 24 jam terakhir (sampai sekarang), bukan dikirim kosong.
	// Di mode mock, kosong tetap berarti semua dummy (data dummy
	// memang historis, bukan 24 jam terakhir).
	if source == gobiz.SourceGobiz {
		now := time.Now().UTC()
		if startRaw == "" {
			startRaw = now.Add(-24 * time.Hour).Format(time.RFC3339)
			start, _ = time.Parse(time.RFC3339, startRaw)
		}
		if endRaw == "" {
			endRaw = now.Format(time.RFC3339)
			end, _ = time.Parse(time.RFC3339, endRaw)
		}
	}
	var mutations []types.Mutasi
	skipped := 0
	if source == gobiz.SourceGobiz {
		muts, sk, err := s.cfg.Gobiz.FetchTransactionsDetailed(c.Context(), gobiz.FetchOptions{StartTime: startRaw, EndTime: endRaw})
		if err != nil {
			code := "sumber-gobiz-error"
			httpStatus := 0
			var ge *gobiz.Error
			if errors.As(err, &ge) {
				code = string(ge.Code)
				httpStatus = ge.HTTPStatus
			}
			return c.Status(500).JSON(fiber.Map{"error": "sumber-gobiz-error", "gobiz_code": code, "gobiz_http_status": httpStatus})
		}
		mutations = muts
		skipped = sk
	} else {
		raw, err := mock.LoadMutationsRaw(s.cfg.DataDir)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "sumber-mock-error"})
		}
		for _, r := range raw {
			mu, ok := mutasiDariRaw(r)
			if !ok {
				skipped++
				continue
			}
			if t, err := time.Parse(time.RFC3339, mu.TransactedAt); err == nil {
				if !start.IsZero() && t.Before(start) {
					continue
				}
				if !end.IsZero() && t.After(end) {
					continue
				}
			}
			mutations = append(mutations, mu)
		}
	}
	if mutations == nil {
		mutations = []types.Mutasi{}
	}
	// Terbaru dulu, supaya pembayaran terakhir langsung terlihat.
	sort.SliceStable(mutations, func(i, j int) bool {
		ti, ei := time.Parse(time.RFC3339, mutations[i].TransactedAt)
		tj, ej := time.Parse(time.RFC3339, mutations[j].TransactedAt)
		if ei == nil && ej == nil {
			return ti.After(tj)
		}
		return mutations[i].TransactedAt > mutations[j].TransactedAt
	})
	return c.JSON(fiber.Map{
		"source":     string(source),
		"mutations":  mutations,
		"count":      len(mutations),
		"skipped":    skipped,
		"start_time": startRaw,
		"end_time":   endRaw,
		"note":       "Riwayat baca saja; tidak memajukan klaim, tidak mengkredit, tidak mengubah status order.",
	})
}

func (s *Server) handleUI(c fiber.Ctx) error {
	html, err := os.ReadFile(s.cfg.UIPath)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{
			"error": "not-found", "detail": "Halaman UI tidak tersedia.",
		})
	}
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.Send(html)
}

func (s *Server) handleUIConfig(c fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"source": func() string {
			if s.cfg.Source == "" {
				return "mock"
			}
			return s.cfg.Source
		}(),
		"port":                s.cfg.Port,
		"poll_interval_ms":    s.cfg.PollIntervalMs,
		"poll_enabled":        s.cfg.PollEnabled,
		"mdr_duration_ms":     s.cfg.MDRDurationMs,
		"unique_code_min":     mdr.UniqueCodeMin,
		"unique_code_max":     mdr.UniqueCodeMax,
		"db_configured":       s.cfg.DatabaseURL != "",
		"session_file_exists": config.SessionFileExists(s.cfg.Gobiz),
		"note":                "Hanya nilai aman. Konfigurasi tidak bisa diubah dari UI di versi ini.",
	})
}

type reserveBody struct {
	OrderID string `json:"order_id"`
	Price   int64  `json:"price"`
}

func (s *Server) handleReserve(requireToken bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		if requireToken {
			token := s.cfg.PollerInternalToken
			if token == "" {
				return c.Status(500).JSON(fiber.Map{
					"error":  "konfigurasi-tidak-valid",
					"detail": "POLLER_INTERNAL_TOKEN belum di-set; endpoint MDR tidak aktif.",
				})
			}
			if c.Get("Authorization") != "Bearer "+token {
				return c.Status(401).JSON(fiber.Map{
					"error": "unauthorized", "detail": "Token internal tidak valid.",
				})
			}
		}
		var body reserveBody
		if err := c.Bind().JSON(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{
				"error": "input-tidak-valid", "detail": "Body harus JSON.",
			})
		}
		if body.OrderID == "" || body.Price <= 0 {
			return c.Status(400).JSON(fiber.Map{
				"error":  "input-tidak-valid",
				"detail": "order_id string non-kosong dan price integer positif wajib.",
			})
		}
		if s.store == nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "konfigurasi-tidak-valid", "detail": "Store belum dibuka.",
			})
		}
		r, err := reserve.ReserveTotal(s.store, reserve.Options{
			OrderID: body.OrderID, Price: body.Price,
			Duration: time.Duration(s.cfg.MDRDurationMs) * time.Millisecond,
		})
		if err != nil {
			var kembar *mdr.TotalKembarManualError
			if errors.As(err, &kembar) {
				return c.Status(409).JSON(fiber.Map{
					"error": "total-kembar-manual", "detail": err.Error(),
				})
			}
			return c.Status(400).JSON(fiber.Map{
				"error": "input-tidak-valid", "detail": "Gagal reservasi.",
			})
		}
		return c.JSON(fiber.Map{
			"order_id": r.OrderID, "price": r.Price, "fee": r.Fee,
			"unique_code": r.UniqueCode, "total": r.Total,
			"total_reserved": r.TotalReserved, "expires_at": r.ExpiresAt,
			"created_at": r.CreatedAt,
			"breakdown": fiber.Map{
				"price": r.Price, "fee": r.Fee,
				"unique_code": r.UniqueCode, "total": r.Total,
			},
			"note": "Reservasi uji saja, bukan kredit. Saldo tidak berubah sampai admin mengonfirmasi.",
		})
	}
}

// ---------- Push notifikasi order dari backend (2026-10-29) ----------
//
// POST /notify-order menerima pemberitahuan order baru dari backend
// mitra-tools (push, perintah Hanif). Endpoint ini TIDAK mengubah
// saldo/status sendiri dan TIDAK mengkredit: ia hanya mencatat
// order yang diberitahukan, lalu MEMICU satu siklus wire
// (subprocess gopay-merchant-go-wire.exe, bila binary-nya ada di folder
// kerja server) secara async — respons tidak menunggu siklus itu
// selesai. Settlement tetap dilakukan wire itu lewat endpoint
// internal mitra-tools, sama seperti loop. Bila binary tidak ada
// atau wire sedang berjalan, respons tetap 200 dengan
// triggered=false dan loop wire (tiap 3 detik) tetap cadangan.
// Endpoint ini ALAT TES LOKAL (tanpa auth, seperti /ui/reserve):
// jangan expose port server ke jaringan bersama tanpa auth /
// jaringan privat.
//
// GET /notified-orders mengembalikan daftar order yang masuk lewat
// push (field aman saja), untuk bukti diagnostik bahwa push sampai.

// notifyBody adalah payload push dari backend. Semua angka integer
// rupiah; total WAJIB = price + mdr + fee (bila tidak sama,
// notifikasi ditolak 400 — loop tetap cadangan, order tidak hilang
// karena order tetap PENDING di backend).
type notifyBody struct {
	OrderID      string `json:"order_id"`
	Total        int64  `json:"total"`
	Price        int64  `json:"price"`
	Mdr          int64  `json:"mdr"`
	Fee          int64  `json:"fee"`
	CreditAmount int64  `json:"credit_amount"`
	ExpiresAt    string `json:"expires_at"`
	CreatedAt    string `json:"created_at"`
}

// triggerWire membangunkan loop wire internal bila jalan (jalur
// utama sejak server + wire jadi 1 proses); bila tidak, fallback
// ke subprocess wire satu siklus (jalur lama).
func (s *Server) triggerWire() bool {
	s.mu.Lock()
	wake := s.wireWake
	s.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
			// Loop sudah bangun / sedang jalan — tetap dianggap
			// terpicu, karena siklus berikutnya jalan segera.
		}
		return true
	}
	return s.triggerWireOnce()
}

func (s *Server) triggerWireOnce() bool {
	s.mu.Lock()
	if s.wireRunning {
		s.mu.Unlock()
		return false
	}
	binary := os.Getenv("WIRE_BINARY_PATH")
	if binary == "" {
		binary = "./gopay-merchant-go-wire.exe"
	}
	if _, err := os.Stat(binary); err != nil {
		s.mu.Unlock()
		return false
	}
	s.wireRunning = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.wireRunning = false
			s.mu.Unlock()
		}()
		cmd := exec.Command(binary)
		// Output wire satu siklus dibuang: buktinya ada di
		// wire-decisions.log yang ditulis wire itu sendiri.
		_ = cmd.Run()
	}()
	return true
}

func (s *Server) handleNotifyOrder(c fiber.Ctx) error {
	var body notifyBody
	if err := c.Bind().JSON(&body); err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "input-tidak-valid", "detail": "Body harus JSON.",
		})
	}
	if body.OrderID == "" || body.Total <= 0 || body.Price < 0 || body.Mdr < 0 || body.Fee < 0 {
		return c.Status(400).JSON(fiber.Map{
			"error":  "input-tidak-valid",
			"detail": "order_id non-kosong dan total positif wajib; price/mdr/fee tidak boleh negatif.",
		})
	}
	if body.Total != body.Price+body.Mdr+body.Fee {
		return c.Status(400).JSON(fiber.Map{
			"error":  "input-tidak-valid",
			"detail": "total harus = price + mdr + fee.",
		})
	}
	s.mu.Lock()
	if s.notified == nil {
		s.notified = make(map[string]NotifiedOrder)
	}
	s.notified[body.OrderID] = NotifiedOrder{
		OrderID: body.OrderID, Total: body.Total, Price: body.Price,
		Mdr: body.Mdr, Fee: body.Fee, CreditAmount: body.CreditAmount,
		ExpiresAt: body.ExpiresAt, CreatedAt: body.CreatedAt,
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.mu.Unlock()
	triggered := s.triggerWire()
	return c.JSON(fiber.Map{
		"ok": true, "order_id": body.OrderID, "triggered": triggered,
		"note": "Notifikasi dicatat. Settlement tetap lewat wire loop (internal proses ini bila jalan, bila tidak lewat subprocess fallback).",
	})
}

func (s *Server) handleNotifiedOrders(c fiber.Ctx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]NotifiedOrder, 0, len(s.notified))
	for _, v := range s.notified {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ReceivedAt < list[j].ReceivedAt })
	return c.JSON(fiber.Map{"orders": list, "count": len(list)})
}

// StartLoop menjalankan loop polling opsional di goroutine: siklus
// yang sama seperti GET /proposals, dengan backoff bila error.
// Loop berhenti bila ctx dibatalkan. Loop MATI secara default;
// aktif hanya bila PollEnabled true (diatur pemanggil/main).
func (s *Server) StartLoop(ctx context.Context) {
	if !s.cfg.PollEnabled {
		return
	}
	go func() {
		consecutiveFailures := int64(0)
		for {
			outcome := s.RunCycle(ctx)
			if outcome.hadError {
				consecutiveFailures++
			} else {
				consecutiveFailures = 0
			}
			delay := s.cfg.PollIntervalMs
			if outcome.hadError {
				delay = match.NextBackoffMs(consecutiveFailures)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(delay) * time.Millisecond):
			}
		}
	}()
}

// ListenAddr mengembalikan alamat listen server (127.0.0.1:PORT).
func (s *Server) ListenAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
}
