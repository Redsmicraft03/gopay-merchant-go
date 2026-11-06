// Package store adalah store persisten poller dengan GORM + Postgres,
// plus MemoryStore untuk tes dan mode demo.
//
// Alasan memakai Postgres (bukan SQLite TS lama): proyek ini dimigrasi
// ke gaya backend Hanif (Go + Fiber + GORM + Postgres), dan store
// harus bisa dipakai lebih dari satu instance bila nanti diperlukan.
// Redis tetap bisa jadi pengganti nanti, tapi itu di luar tugas ini.
//
// Isi store di sini hanya ID order/transaksi dan nominal — TIDAK ADA
// HP/OTP/token/QRIS/secret.
//
// Tabel (nama default GORM):
//   - reservations: satu baris per order yang di-reserve (total unik).
//     status selalu "reserved"; keaktifan dinilai dari expires_at vs
//     waktu evaluasi (bukan dari status), supaya reservasi kedaluwarsa
//     otomatis tidak aktif tanpa perlu job pembersih.
//   - claims: satu baris per transaction_id yang sudah diusulkan.
//     PRIMARY KEY transaction_id menjamin klaim sekali, bahkan bila
//     proses restart dan siklus diulang.
//
// PERBEDAAN SADAR dari store SQLite TS: penyimpanan reservasi di Go
// memakai UPSERT (GORM Save / map overwrite di memori). Di TS,
// INSERT biasa akan gagal bila order yang sama pernah di-reserve
// lalu kedaluwarsa dan di-reserve ulang — itu bug TS, bukan perilaku
// yang dipertahankan. Di Go, reservasi ulang setelah kedaluwarsa
// menggantikan baris lama order itu (bukan membuat duplikat).
//
// PERINGATAN: tes di sandbox TIDAK memakai Postgres live (tidak ada
// server Postgres di sini). AutoMigrate + store Postgres nyata BELUM
// teruji live dan HARUS dijalankan di laptop setelah transfer, dengan
// DATABASE_URL laptop sendiri. Lihat README.
package store

import (
	"errors"
	"sync"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Reservation adalah satu reservasi total bayar.
type Reservation struct {
	OrderID       string `gorm:"primaryKey" json:"order_id"`
	Price         int64  `json:"price"`
	Fee           int64  `json:"fee"`
	UniqueCode    int64  `json:"unique_code"`
	TotalReserved int64  `json:"total_reserved"`
	Status        string `json:"status"`
	ExpiresAt     string `json:"expires_at"`
	CreatedAt     string `json:"created_at"`
}

// Claim adalah satu klaim transaksi yang sudah diusulkan.
type Claim struct {
	TransactionID string `gorm:"primaryKey" json:"transaction_id"`
	OrderID       string `json:"order_id"`
	ClaimedAt     string `json:"claimed_at"`
}

// Store adalah antarmuka store yang dipakai reserve dan server.
// Implementasinya: MemoryStore (tes/demo) atau PostgresStore (produksi).
type Store interface {
	InsertReservation(r *Reservation) error
	GetReservationByOrder(orderID string) (*Reservation, error)
	// GetActiveReservationByTotal mengembalikan reservasi aktif
	// (status reserved DAN expires_at > now) untuk total itu, atau
	// nil bila tidak ada.
	GetActiveReservationByTotal(total int64, now time.Time) (*Reservation, error)
	ListReservations() ([]*Reservation, error)
	InsertClaim(transactionID, orderID, claimedAt string) error
	IsClaimed(transactionID string) (bool, error)
	ListClaimed() ([]string, error)
	ListClaims() ([]*Claim, error)
	Close() error
}

// ---------- MemoryStore ----------

// MemoryStore adalah store in-memory (mutex), untuk tes dan untuk
// server bila DATABASE_URL kosong dalam mode demo. Klaim/reservasi
// HILANG saat proses restart — jangan pakai ini di produksi.
type MemoryStore struct {
	mu           sync.Mutex
	reservations map[string]*Reservation
	claims       map[string]*Claim
}

// NewMemoryStore membuat MemoryStore kosong.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		reservations: make(map[string]*Reservation),
		claims:       make(map[string]*Claim),
	}
}

func copyReservation(r *Reservation) *Reservation {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func (s *MemoryStore) InsertReservation(r *Reservation) error {
	if r == nil || r.OrderID == "" {
		return errors.New("order_id tidak boleh kosong")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservations[r.OrderID] = copyReservation(r)
	return nil
}

func (s *MemoryStore) GetReservationByOrder(orderID string) (*Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyReservation(s.reservations[orderID]), nil
}

func (s *MemoryStore) GetActiveReservationByTotal(total int64, now time.Time) (*Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reservations {
		if r.TotalReserved != total || r.Status != "reserved" {
			continue
		}
		exp, err := time.Parse(time.RFC3339Nano, r.ExpiresAt)
		if err != nil {
			if exp2, err2 := time.Parse(time.RFC3339, r.ExpiresAt); err2 == nil {
				exp = exp2
			} else {
				continue
			}
		}
		if exp.After(now) {
			return copyReservation(r), nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) ListReservations() ([]*Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Reservation, 0, len(s.reservations))
	for _, r := range s.reservations {
		out = append(out, copyReservation(r))
	}
	return out, nil
}

func (s *MemoryStore) InsertClaim(transactionID, orderID, claimedAt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.claims[transactionID]; !exists {
		s.claims[transactionID] = &Claim{TransactionID: transactionID, OrderID: orderID, ClaimedAt: claimedAt}
	}
	return nil
}

func (s *MemoryStore) IsClaimed(transactionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.claims[transactionID]
	return ok, nil
}

func (s *MemoryStore) ListClaimed() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.claims))
	for id := range s.claims {
		out = append(out, id)
	}
	return out, nil
}

func (s *MemoryStore) ListClaims() ([]*Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Claim, 0, len(s.claims))
	for _, c := range s.claims {
		cc := *c
		out = append(out, &cc)
	}
	return out, nil
}

// Close tidak melakukan apa-apa untuk MemoryStore.
func (s *MemoryStore) Close() error { return nil }

// ---------- PostgresStore ----------

// PostgresStore adalah store GORM + Postgres. Buka dengan
// OpenPostgres; jangan panggil NewPostgresStore langsung dari luar
// tanpa AutoMigrate (OpenPostgres sudah melakukannya).
type PostgresStore struct {
	db *gorm.DB
}

// OpenPostgres membuka koneksi Postgres dari DSN (DATABASE_URL) dan
// menjalankan AutoMigrate untuk reservations dan claims.
// BELUM teruji live di sandbox (tidak ada server Postgres di sini).
func OpenPostgres(dsn string) (*PostgresStore, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&Reservation{}, &Claim{}); err != nil {
		return nil, err
	}
	return &PostgresStore{db: db}, nil
}

// DB mengembalikan *gorm.DB yang dipakai store ini (untuk tes /
// operasi lanjutan yang disetujui pemanggil).
func (s *PostgresStore) DB() *gorm.DB { return s.db }

func (s *PostgresStore) InsertReservation(r *Reservation) error {
	if r == nil || r.OrderID == "" {
		return errors.New("order_id tidak boleh kosong")
	}
	// Save = upsert berdasarkan primary key (lihat catatan paket).
	return s.db.Save(r).Error
}

func (s *PostgresStore) GetReservationByOrder(orderID string) (*Reservation, error) {
	var r Reservation
	err := s.db.First(&r, "order_id = ?", orderID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PostgresStore) GetActiveReservationByTotal(total int64, now time.Time) (*Reservation, error) {
	var rows []*Reservation
	if err := s.db.Where("total_reserved = ? AND status = ?", total, "reserved").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		exp, err := time.Parse(time.RFC3339Nano, r.ExpiresAt)
		if err != nil {
			if exp2, err2 := time.Parse(time.RFC3339, r.ExpiresAt); err2 == nil {
				exp = exp2
			} else {
				continue
			}
		}
		if exp.After(now) {
			return r, nil
		}
	}
	return nil, nil
}

func (s *PostgresStore) ListReservations() ([]*Reservation, error) {
	var rows []*Reservation
	if err := s.db.Order("created_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *PostgresStore) InsertClaim(transactionID, orderID, claimedAt string) error {
	// INSERT ... ON CONFLICT DO NOTHING: klaim sekali per transaksi.
	return s.db.Exec(
		"INSERT INTO claims (transaction_id, order_id, claimed_at) VALUES (?, ?, ?) ON CONFLICT (transaction_id) DO NOTHING",
		transactionID, orderID, claimedAt,
	).Error
}

func (s *PostgresStore) IsClaimed(transactionID string) (bool, error) {
	var count int64
	if err := s.db.Model(&Claim{}).Where("transaction_id = ?", transactionID).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *PostgresStore) ListClaimed() ([]string, error) {
	var ids []string
	if err := s.db.Model(&Claim{}).Pluck("transaction_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *PostgresStore) ListClaims() ([]*Claim, error) {
	var rows []*Claim
	if err := s.db.Order("claimed_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Close menutup koneksi database di bawah GORM.
func (s *PostgresStore) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
