package reserve

import (
	"errors"
	"testing"
	"time"

	"mitrain-gopay/internal/mdr"
	"mitrain-gopay/internal/store"
)

func fixedNow() time.Time {
	t, _ := time.Parse(time.RFC3339, "2026-10-28T10:00:00+07:00")
	return t
}

func TestReserveIdempotent(t *testing.T) {
	st := store.NewMemoryStore()
	now := fixedNow()
	r1, err := ReserveTotal(st, Options{OrderID: "ORDER-1", Price: 1000, Now: now, CodeGenerator: func() (int64, error) { return 1216, nil }})
	if err != nil {
		t.Fatalf("reserve pertama: %v", err)
	}
	if r1.Total != 2216 || r1.UniqueCode != 1216 || r1.Fee != 0 {
		t.Fatalf("reserve = %+v, mau total 2216 kode 1216 fee 0", r1)
	}
	r2, err := ReserveTotal(st, Options{OrderID: "ORDER-1", Price: 1000, Now: now, CodeGenerator: func() (int64, error) { return 999, nil }})
	if err != nil {
		t.Fatalf("reserve kedua (idempotent): %v", err)
	}
	if r2.Total != r1.Total || r2.UniqueCode != r1.UniqueCode {
		t.Fatalf("reserve kedua = %+v, mau sama dengan pertama %+v", r2, r1)
	}
}

func TestReserveTabrakanJadiManual(t *testing.T) {
	st := store.NewMemoryStore()
	now := fixedNow()
	if _, err := ReserveTotal(st, Options{OrderID: "ORDER-1", Price: 1000, Now: now, CodeGenerator: func() (int64, error) { return 500, nil }}); err != nil {
		t.Fatalf("reserve pertama: %v", err)
	}
	_, err := ReserveTotal(st, Options{OrderID: "ORDER-2", Price: 1000, Now: now, CodeGenerator: func() (int64, error) { return 500, nil }})
	var kembar *mdr.TotalKembarManualError
	if !errors.As(err, &kembar) {
		t.Fatalf("reserve tabrakan: err = %v, mau TotalKembarManualError", err)
	}
}

func TestReserveKedaluarsaMembebaskanTotal(t *testing.T) {
	st := store.NewMemoryStore()
	now := fixedNow()
	if _, err := ReserveTotal(st, Options{OrderID: "ORDER-1", Price: 1000, Now: now, CodeGenerator: func() (int64, error) { return 500, nil }}); err != nil {
		t.Fatalf("reserve pertama: %v", err)
	}
	// Setelah 15 menit lewat, total yang sama boleh dipakai order lain.
	later := now.Add(16 * time.Minute)
	r, err := ReserveTotal(st, Options{OrderID: "ORDER-2", Price: 1000, Now: later, CodeGenerator: func() (int64, error) { return 500, nil }})
	if err != nil {
		t.Fatalf("reserve setelah kedaluwarsa: %v", err)
	}
	if r.Total != 1500 {
		t.Fatalf("total = %d, mau 1500", r.Total)
	}
	// Order lama yang kedaluwarsa lalu di-reserve ulang juga bisa
	// (upsert Go, bukan INSERT gagal seperti SQLite TS).
	r2, err := ReserveTotal(st, Options{OrderID: "ORDER-1", Price: 1000, Now: later, CodeGenerator: func() (int64, error) { return 700, nil }})
	if err != nil {
		t.Fatalf("reserve ulang order kedaluwarsa: %v", err)
	}
	if r2.Total != 1700 {
		t.Fatalf("total ulang = %d, mau 1700", r2.Total)
	}
}
