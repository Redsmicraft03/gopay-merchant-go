// Package types berisi bentuk data bersama poller, wire, dan server.
// Semua nominal adalah integer rupiah (int64), tanpa float dan tanpa
// toleransi selisih. Semua timestamp adalah string ISO 8601.
// Tidak ada field nomor HP, OTP, token, atau QRIS di tipe mana pun.
package types

// Decision adalah keputusan audit per pasangan (order, transaksi).
type Decision string

const (
	DecisionProposed    Decision = "DIUSULKAN"
	DecisionNotProposed Decision = "TIDAK_DIUSULKAN"
	DecisionSkipped     Decision = "POLLING_DILEWATI"
	DecisionError       Decision = "ERROR"
)

// Reason adalah alasan keputusan, sesuai kontrak TS.
type Reason string

const (
	ReasonExactAmount    Reason = "nominal-persis"
	ReasonAmountMismatch Reason = "nominal-tidak-cocok"
	ReasonOutOfWindow    Reason = "di-luar-rentang-waktu"
	ReasonAlreadyClaimed Reason = "sudah-diklaim"
	ReasonExpired        Reason = "kedaluwarsa"
	ReasonNoPending      Reason = "tidak-ada-order-pending"
	ReasonMDRMismatch    Reason = "mdr-tidak-cocok"
	ReasonInvalidInput   Reason = "input-tidak-valid"
	ReasonMockError      Reason = "sumber-mock-error"
	ReasonGobizError     Reason = "sumber-gobiz-error"
	ReasonNoMatch        Reason = "tidak-cocok"
)

// OrderPending adalah order PENDING dari file dummy, endpoint internal
// mitra-tools, atau jembatan POC (setelah konversi).
type OrderPending struct {
	OrderID       string `json:"order_id"`
	TotalSnapshot int64  `json:"total_snapshot"`
	CreatedAt     string `json:"created_at"`
	ExpiryAt      string `json:"expiry_at"`
	GraceEndAt    string `json:"grace_end_at"`
	Status        string `json:"status"`
	// Rincian total, OPSIONAL, hanya untuk cek identitas MDR eksplisit.
	// GoBiz TIDAK mengirim field MDR, jadi MDR mutasi hanya bisa
	// diturunkan: mdr_tersirat = amount - price - fee. Bila backend
	// tidak mengirim ketiganya, cek MDR dilewati (mdr_cocok null)
	// dan pencocokan kembali ke total_snapshot saja. Nilai ini tidak
	// boleh diisi dari tebakan: hanya dari data order backend.
	Price *int64 `json:"price,omitempty"`
	Fee   *int64 `json:"fee,omitempty"`
	MDR   *int64 `json:"mdr,omitempty"`
}

// Mutasi adalah satu transaksi GoBiz (atau mock) yang sudah dipetakan.
// Status dan PaymentType adalah elemen trace: status GoBiz apa adanya
// (mis. SETTLEMENT/CAPTURE, lowercase bila aslinya begitu) dan tipe
// pembayaran (mis. QRIS/GOPAY). Keduanya opsional: bila sumber tidak
// mengirimnya, field kosong dan TIDAK ditebak — mock lama memang
// tidak punya keduanya, jadi omitempty.
type Mutasi struct {
	TransactionID string `json:"transaction_id"`
	Amount        int64  `json:"amount"`
	TransactedAt  string `json:"transacted_at"`
	Source        string `json:"source"`
	Status        string `json:"status,omitempty"`
	PaymentType   string `json:"payment_type,omitempty"`
}

// Proposal adalah usulan kecocokan. Usulan BUKAN kredit saldo, BUKAN
// perubahan status, dan BUKAN perubahan saldo.
type Proposal struct {
	OrderID        string `json:"order_id"`
	TransactionID  string `json:"transaction_id"`
	Amount         int64  `json:"amount"`
	TransactedAt   string `json:"transacted_at"`
	ProposalStatus string `json:"proposal_status"`
	MatchedAt      string `json:"matched_at"`
	MatchRule      string `json:"match_rule"`
	Status         string `json:"status,omitempty"`
	PaymentType    string `json:"payment_type,omitempty"`
}

// AuditLogEntry adalah satu baris log audit. order_id / transaction_id
// boleh null (pointer nil) bila keputusan itu per siklus atau
// pengenalnya sendiri tidak valid / hilang.
type AuditLogEntry struct {
	LoggedAt      string   `json:"logged_at"`
	OrderID       *string  `json:"order_id"`
	TransactionID *string  `json:"transaction_id"`
	Decision      Decision `json:"decision"`
	Reason        Reason   `json:"reason"`
	Amount        *int64   `json:"amount"`
	TransactedAt  *string  `json:"transacted_at"`
	// Cek identitas MDR eksplisit untuk pasangan ini:
	//   MDR order = MDR yang disimpan backend untuk order ini
	//     (nil bila backend tidak mengirimnya).
	//   MDR tersirat = amount mutasi - price - fee
	//     (nil bila price/fee tidak dikirim backend).
	//   MDR cocok = true bila keduanya diketahui dan sama, false
	//     bila keduanya diketahui dan berbeda, nil bila salah satu
	//     tidak diketahui (identitas saat itu hanya lewat total).
	// Pasangan dengan MDR cocok == false TIDAK PERNAH menjadi pemenang.
	MDROrder    *int64 `json:"mdr_order"`
	MDRTersirat *int64 `json:"mdr_tersirat"`
	MDRCocok    *bool  `json:"mdr_cocok"`
	// Elemen trace GoBiz apa adanya (kosong bila sumber tak mengirim).
	Status      string `json:"status,omitempty"`
	PaymentType string `json:"payment_type,omitempty"`
}

// helpers untuk pointer, dipakai paket lain lewat fungsi di bawah.
func StrPtr(s string) *string { return &s }
func IntPtr(v int64) *int64   { return &v }
func BoolPtr(v bool) *bool    { return &v }
