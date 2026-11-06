// Command login adalah script login GoBiz sekali jalan — HANYA
// untuk pemilik akun dummy / pemilik akun yang hadir, di mesinnya
// sendiri.
//
// Pemakaian (dari folder proyek):
//
//	go run ./cmd/login
//
// Alur transparan (dari laporan studi — ditulis ulang, bukan blob
// obfuscate):
//  1. Minta nomor HP (diketik di terminal, TIDAK disimpan, TIDAK di-log).
//  2. RequestOtp -> POST {auth}/goid/login/request
//     (client_id go-biz-web-new, nomor nasional, country_code 62).
//  3. Minta kode OTP (diketik di terminal, TIDAK disimpan, TIDAK di-log).
//  4. VerifyOtp -> POST {auth}/goid/token (grant_type otp).
//  5. FetchMerchantConfig -> GET {auth}/goresto/v5/public/users/config
//     untuk mengambil merchant_id / outlet_name OTOMATIS. Bila langkah
//     ini GAGAL, GOBIZ_MERCHANT_ID dari env dipakai sebagai FALLBACK
//     saja; bila fallback itu pun kosong, sesi tetap disimpan tanpa
//     merchant dan polling nanti gagal jelas (bukan menebak ID).
//  6. SaveSession ke file dari env GOBIZ_SESSION_FILE
//     (default .gobiz-session.json di folder proyek). File sesi
//     HANYA berisi accessToken, refreshToken, expiresAt, dan bila
//     ada merchantId/outletName — TIDAK ada nomor HP / OTP.
//
// PENTING:
//   - Auth ini MENIRU dashboard web GoBiz, BUKAN API partner resmi.
//     Penerimaan live atas kombinasi client/version ini MASIH BELUM
//     TERBUKTI; uji live hanya dengan akun dummy, bukan akun produksi.
//   - Script ini TIDAK melakukan transfer, TIDAK mengubah saldo/status,
//     TIDAK mengakses database mitra-tools, TIDAK auto-credit.
//   - File sesi TIDAK BOLEH di-commit atau dikirim ke siapa pun;
//     token di dalamnya plaintext.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"gopay-merchant-go/internal/gobiz"
)

func ask(reader *bufio.Reader, question string) (string, error) {
	fmt.Print(question)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func main() {
	// .env di folder kerja dibaca bila ada; env manual tetap menang.
	_ = godotenv.Load()
	cfg := gobiz.ConfigFromEnv()
	ctx := context.Background()
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Login GoBiz (read-only, sesi disimpan lokal saja).")
	fmt.Printf("File sesi tujuan: %s\n", cfg.SessionFile)
	if cfg.MerchantID == "" {
		fmt.Println("Catatan: GOBIZ_MERCHANT_ID kosong — merchant akan diambil otomatis dari langkah config; bila itu gagal, polling nanti gagal jelas.")
	}

	phone, err := ask(reader, "Nomor HP GoBiz: ")
	if err != nil || phone == "" {
		fmt.Fprintln(os.Stderr, "Nomor HP kosong, dibatalkan.")
		os.Exit(1)
	}
	otpResult, err := cfg.RequestOtp(ctx, phone)
	// Hapus nomor HP dari memori sejauh yang bisa dilakukan di Go:
	// variabel phone tidak dipakai lagi setelah titik ini.
	phone = ""
	if err != nil {
		fmt.Fprintf(os.Stderr, "Gagal request OTP: %s\n", err.Error())
		os.Exit(1)
	}
	fmt.Printf("OTP sudah diminta (panjang %d digit). Cek HP-mu, lalu masukkan kode OTP di bawah.\n", otpResult.OtpLength)
	otpCode, err := ask(reader, "Kode OTP: ")
	if err != nil || otpCode == "" {
		fmt.Fprintln(os.Stderr, "Kode OTP kosong, dibatalkan. Token OTP tidak disimpan.")
		os.Exit(1)
	}
	session, err := cfg.VerifyOtp(ctx, otpResult.OtpToken, otpCode)
	otpCode = ""
	if err != nil {
		var ge *gobiz.Error
		code := "sumber-gobiz-error"
		if errors.As(err, &ge) {
			code = string(ge.Code)
		}
		fmt.Fprintf(os.Stderr, "Gagal verifikasi OTP (%s): %s\n", code, err.Error())
		os.Exit(1)
	}
	// Langkah 3: merchant otomatis; gagal -> fallback env, bukan tebak.
	merchant, err := cfg.FetchMerchantConfig(ctx, session)
	if err != nil {
		var ge *gobiz.Error
		code := "sumber-gobiz-error"
		if errors.As(err, &ge) {
			code = string(ge.Code)
		}
		fmt.Fprintf(os.Stderr, "Peringatan: config merchant gagal (%s); memakai fallback bila ada.\n", code)
		if cfg.MerchantID != "" {
			session.MerchantID = cfg.MerchantID
			fmt.Println("Fallback GOBIZ_MERCHANT_ID dipakai untuk sesi ini.")
		} else {
			fmt.Fprintln(os.Stderr, "Peringatan: tidak ada merchant dari config maupun fallback; polling nanti gagal jelas sampai merchant diisi.")
		}
	} else {
		session.MerchantID = merchant.MerchantID
		session.OutletName = merchant.OutletName
		fmt.Println("Merchant diambil otomatis dari config.")
	}
	if err := gobiz.SaveSession(cfg.SessionFile, session); err != nil {
		fmt.Fprintf(os.Stderr, "Gagal menyimpan sesi: %s\n", err.Error())
		os.Exit(1)
	}
	fmt.Println("Login berhasil. Sesi tersimpan di file lokal service.")
	fmt.Println("Langkah berikutnya: jalankan server dengan SOURCE=gobiz, lalu cek /proposals.")
}
