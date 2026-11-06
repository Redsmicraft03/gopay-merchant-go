// Package mdr berisi rumus MDR + kode unik POC untuk module GoPay
// (jembatan, BUKAN kontrak global).
//
// ISTILAH YANG DIKUNCI HANIF (2026-10-28, koreksi atas rumus lama):
//
//	MDR = kode unik (199-1999, direservasi per order).
//	Fee = 0,3% x harga, hanya bila harga > 500.000 (di bawah itu 0).
//	Total bayar = harga + MDR (kode unik) + fee (bila ada).
//	Contoh yang harus benar:
//	- harga 1.000.000, kode unik 500  -> MDR 500, fee 3.000, total 1.003.500
//	- harga   100.000, kode unik 500  -> MDR 500, fee     0, total   100.500
//	- harga   600.000, kode unik 1000 -> MDR 1000, fee 1.800, total  602.800
//
// Jangan menyebut MDR sebagai gabungan kode unik + fee. Hanya ada
// tiga nilai: harga, MDR (= kode unik), fee.
//
// Kode unik (MDR) 199-1999 BUKAN fee GoPay; ia hanya penanda agar
// total bayar tiap order berbeda. Durasi reservasi 15 menit adalah
// KONFIGURASI module GoPay ini, BUKAN mengubah kontrak global
// (60 menit + grace 15 menit) yang berlaku untuk module lain.
// Semua nominal integer rupiah. Tidak ada HP/OTP/token/secret di sini.
package mdr

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
)

const (
	// MDRThreshold adalah batas fee: fee hanya bila harga > 500.000.
	MDRThreshold = int64(500_000)
	// UniqueCodeMin / Max adalah rentang MDR (kode unik).
	UniqueCodeMin = int64(199)
	UniqueCodeMax = int64(1999)
	// ReserveDurationMs adalah durasi reservasi MDR (15 menit).
	ReserveDurationMs = int64(15 * 60 * 1000)
	// MaxUniqueAttempts adalah percobaan regenerasi kode bila tabrakan.
	MaxUniqueAttempts = 20
)

// Breakdown adalah rincian total bayar POC. Field UniqueCode ADALAH
// MDR — bukan nilai terpisah dari MDR, dan bukan gabungan dengan fee.
type Breakdown struct {
	Price      int64 `json:"price"`
	Fee        int64 `json:"fee"`
	UniqueCode int64 `json:"unique_code"`
	Total      int64 `json:"total"`
}

// FeeFor menghitung fee: 0 bila harga <= 500.000, bila tidak
// ceil(harga * 0,3%) = ceil(harga * 3 / 1000).
func FeeFor(price int64) (int64, error) {
	if price <= 0 {
		return 0, errors.New("harga paket harus integer positif")
	}
	if price <= MDRThreshold {
		return 0, nil
	}
	return (price*3 + 999) / 1000, nil
}

// BreakdownFor menghitung total bayar POC: harga + MDR + fee.
func BreakdownFor(price, uniqueCode int64) (Breakdown, error) {
	if uniqueCode < UniqueCodeMin || uniqueCode > UniqueCodeMax {
		return Breakdown{}, fmt.Errorf("kode unik harus integer 199-1999")
	}
	fee, err := FeeFor(price)
	if err != nil {
		return Breakdown{}, err
	}
	return Breakdown{Price: price, Fee: fee, UniqueCode: uniqueCode, Total: price + uniqueCode + fee}, nil
}

// RandomUniqueCode menghasilkan kode unik acak 199-1999 dari
// crypto/rand (BUKAN math/rand, supaya tidak mudah ditebak).
func RandomUniqueCode() (int64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(UniqueCodeMax-UniqueCodeMin+1))
	if err != nil {
		return 0, err
	}
	return UniqueCodeMin + n.Int64(), nil
}

// TotalKembarManualError adalah error fallback manual: total kembar,
// JANGAN auto-credit. Pemanggil WAJIB fallback manual.
type TotalKembarManualError struct{}

func (e *TotalKembarManualError) Error() string {
	return "Total bayar sudah dipakai order aktif lain; lanjutkan manual, jangan auto-credit."
}

// Code adalah kode error yang dipakai pemanggil (mis. HTTP 409).
func (e *TotalKembarManualError) Code() string { return "total-kembar-manual" }
