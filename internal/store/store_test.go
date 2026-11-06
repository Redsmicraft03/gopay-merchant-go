package store

import (
	"testing"
	"time"
)

// Tes ini hanya MemoryStore (tanpa Postgres live). Store Postgres
// nyata (AutoMigrate) BELUM teruji live di sandbox — lihat README.
func TestMemoryStoreReservationDanClaim(t *testing.T) {
	st := NewMemoryStore()
	now, _ := time.Parse(time.RFC3339, "2026-10-28T10:00:00+07:00")
	r := &Reservation{
		OrderID: "ORDER-1", Price: 1000, Fee: 0, UniqueCode: 1216,
		TotalReserved: 2216, Status: "reserved",
		ExpiresAt: "2026-10-28T10:15:00+07:00", CreatedAt: "2026-10-28T10:00:00+07:00",
	}
	if err := st.InsertReservation(r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := st.GetReservationByOrder("ORDER-1")
	if err != nil || got == nil || got.TotalReserved != 2216 {
		t.Fatalf("get = %+v err = %v", got, err)
	}
	active, err := st.GetActiveReservationByTotal(2216, now)
	if err != nil || active == nil || active.OrderID != "ORDER-1" {
		t.Fatalf("active = %+v err = %v", active, err)
	}
	later := now.Add(20 * time.Minute)
	active, _ = st.GetActiveReservationByTotal(2216, later)
	if active != nil {
		t.Fatalf("setelah kedaluwarsa active harus nil, dapat %+v", active)
	}
	if err := st.InsertClaim("TX-001", "ORDER-1", "2026-10-28T10:10:00+07:00"); err != nil {
		t.Fatalf("insert claim: %v", err)
	}
	if err := st.InsertClaim("TX-001", "ORDER-1", "2026-10-28T10:10:00+07:00"); err != nil {
		t.Fatalf("insert claim duplikat harus di-ignore, bukan error: %v", err)
	}
	claimed, _ := st.IsClaimed("TX-001")
	if !claimed {
		t.Fatal("TX-001 harus claimed")
	}
	ids, _ := st.ListClaimed()
	if len(ids) != 1 || ids[0] != "TX-001" {
		t.Fatalf("list claimed = %v, mau [TX-001]", ids)
	}
}
