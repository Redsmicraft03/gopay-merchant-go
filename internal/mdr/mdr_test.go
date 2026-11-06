package mdr

import "testing"

func TestFeeFor(t *testing.T) {
	cases := []struct {
		price int64
		fee   int64
	}{
		{1000, 0},
		{100000, 0},
		{500000, 0},
		{500001, 1501},
		{600000, 1800},
		{1000000, 3000},
	}
	for _, c := range cases {
		got, err := FeeFor(c.price)
		if err != nil || got != c.fee {
			t.Fatalf("FeeFor(%d) = %d, %v; mau %d", c.price, got, err, c.fee)
		}
	}
	if _, err := FeeFor(0); err == nil {
		t.Fatal("FeeFor(0) harus error")
	}
}

func TestBreakdownForContohKunci(t *testing.T) {
	cases := []struct {
		price, code, fee, total int64
	}{
		{1000000, 500, 3000, 1003500},
		{100000, 500, 0, 100500},
		{600000, 1000, 1800, 602800},
		{1000, 1216, 0, 2216},
	}
	for _, c := range cases {
		b, err := BreakdownFor(c.price, c.code)
		if err != nil || b.Fee != c.fee || b.Total != c.total || b.UniqueCode != c.code {
			t.Fatalf("BreakdownFor(%d, %d) = %+v, %v; mau fee %d total %d", c.price, c.code, b, err, c.fee, c.total)
		}
	}
	if _, err := BreakdownFor(1000, 198); err == nil {
		t.Fatal("kode 198 (di bawah 199) harus error")
	}
	if _, err := BreakdownFor(1000, 2000); err == nil {
		t.Fatal("kode 2000 (di atas 1999) harus error")
	}
}

func TestRandomUniqueCodeDalamRentang(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := RandomUniqueCode()
		if err != nil || code < UniqueCodeMin || code > UniqueCodeMax {
			t.Fatalf("RandomUniqueCode() = %d, %v; mau 199-1999", code, err)
		}
	}
}
