// Package reserve berisi reservasi total bayar (jembatan POC).
//
// Alur: harga paket -> fee (mdr) -> kode unik random -> total.
// Total dicek ke store: bila sudah dipakai order aktif lain dalam
// jendela, generate ulang sampai dapat yang belum dipakai (maks
// 20 percobaan). Bila tetap tabrakan, kembalikan
// TotalKembarManualError — pemanggil WAJIB fallback manual, JANGAN
// mengusulkan / mengkredit otomatis untuk total itu.
//
// Reservasi yang sudah ada untuk order_id yang sama dan masih aktif
// dikembalikan apa adanya (reserve idempotent per order), supaya
// pemanggilan ulang tidak membuat total baru.
package reserve

import (
	"errors"
	"time"

	"gopay-merchant-go/internal/mdr"
	"gopay-merchant-go/internal/store"
)

// Result adalah hasil reservasi: Reservation + rincian breakdown.
// Field Total sama dengan TotalReserved (dua nama untuk kompatibilitas
// JSON TS: total dan total_reserved).
type Result struct {
	OrderID       string `json:"order_id"`
	Price         int64  `json:"price"`
	Fee           int64  `json:"fee"`
	UniqueCode    int64  `json:"unique_code"`
	Total         int64  `json:"total"`
	TotalReserved int64  `json:"total_reserved"`
	Status        string `json:"status"`
	ExpiresAt     string `json:"expires_at"`
	CreatedAt     string `json:"created_at"`
}

// Options adalah opsi reservasi.
type Options struct {
	OrderID  string
	Price    int64
	Now      time.Time     // Default: sekarang. Di tes diisi waktu dummy.
	Duration time.Duration // Default: 15 menit (konfigurasi module GoPay).
	// CodeGenerator adalah generator kode unik (default: random).
	// Di tes bisa di-inject.
	CodeGenerator func() (int64, error)
}

// ReserveTotal mereserve total bayar untuk satu order.
func ReserveTotal(st store.Store, opts Options) (*Result, error) {
	if opts.OrderID == "" {
		return nil, errors.New("order_id tidak boleh kosong")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	duration := opts.Duration
	if duration == 0 {
		duration = time.Duration(mdr.ReserveDurationMs) * time.Millisecond
	}
	existing, err := st.GetReservationByOrder(opts.OrderID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if exp, err := time.Parse(time.RFC3339Nano, existing.ExpiresAt); err == nil && exp.After(now) {
			return resultFromReservation(existing), nil
		}
		if exp, err := time.Parse(time.RFC3339, existing.ExpiresAt); err == nil && exp.After(now) {
			return resultFromReservation(existing), nil
		}
	}
	createdAt := now.UTC().Format(time.RFC3339)
	expiresAt := now.Add(duration).UTC().Format(time.RFC3339)
	gen := opts.CodeGenerator
	if gen == nil {
		gen = mdr.RandomUniqueCode
	}
	for attempt := 0; attempt < mdr.MaxUniqueAttempts; attempt++ {
		code, err := gen()
		if err != nil {
			continue
		}
		breakdown, err := mdr.BreakdownFor(opts.Price, code)
		if err != nil {
			continue
		}
		clash, err := st.GetActiveReservationByTotal(breakdown.Total, now)
		if err != nil {
			return nil, err
		}
		if clash != nil && clash.OrderID != opts.OrderID {
			continue
		}
		reservation := &store.Reservation{
			OrderID: opts.OrderID, Price: opts.Price, Fee: breakdown.Fee,
			UniqueCode: breakdown.UniqueCode, TotalReserved: breakdown.Total,
			Status: "reserved", ExpiresAt: expiresAt, CreatedAt: createdAt,
		}
		if err := st.InsertReservation(reservation); err != nil {
			// Race / panggil ulang: kembalikan yang tersimpan bila
			// totalnya sama, kalau tidak lanjutkan percobaan berikutnya.
			again, err2 := st.GetReservationByOrder(opts.OrderID)
			if err2 == nil && again != nil && again.TotalReserved == breakdown.Total {
				return resultFromReservation(again), nil
			}
			continue
		}
		return &Result{
			OrderID: opts.OrderID, Price: opts.Price, Fee: breakdown.Fee,
			UniqueCode: breakdown.UniqueCode, Total: breakdown.Total,
			TotalReserved: breakdown.Total, Status: "reserved",
			ExpiresAt: expiresAt, CreatedAt: createdAt,
		}, nil
	}
	return nil, &mdr.TotalKembarManualError{}
}

func resultFromReservation(r *store.Reservation) *Result {
	return &Result{
		OrderID: r.OrderID, Price: r.Price, Fee: r.Fee,
		UniqueCode: r.UniqueCode, Total: r.TotalReserved,
		TotalReserved: r.TotalReserved, Status: r.Status,
		ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt,
	}
}
