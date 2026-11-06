// Package bridge adalah antarmuka pembaca order + pengirim usulan
// untuk mitra-tools — JEMBATAN POC, BUKAN endpoint mitra-tools asli.
//
// Endpoint mitra-tools ASLI (GET pending-orders / POST proposals)
// BELUM DIBANGUN di tugas POC ini dan masih menunggu kunci Bagus +
// perintah terpisah. Yang ada di sini hanyalah bentuk request/response
// yang dipakai poller, diuji melawan MOCK HTTP lokal di tes, bukan
// DB asli dan bukan API asli.
//
// Bentuk mock yang dikunci untuk POC:
//   - GET {base}/api/internal/pending-orders -> array BridgeOrder:
//     { order_id, total_reserved, created_at, expires_at, status }
//     (status yang diproses hanya "PENDING").
//   - POST {base}/api/internal/proposals, body = Proposal, -> 200/201
//     bila diterima. Usulan BUKAN kredit: backend-lah yang memutuskan
//     konfirmasi manual / auto di fase berikutnya (bukan di sini).
//
// Konversi ke OrderPending: total_snapshot = total_reserved,
// expiry_at = expires_at, grace_end_at = expires_at. Artinya jendela
// module GoPay ini adalah reservasi 15 menit itu sendiri, TANPA grace
// tambahan — eksplisit, berbeda dari kontrak lama 60+15 yang berlaku
// untuk module lain. Bila mitra-tools nanti memakai grace, grace itu
// harus dikirim sebagai field terpisah dan konversinya diubah di sini,
// bukan ditebak.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gopay-merchant-go/internal/types"
)

// BridgeOrder adalah order dari mock jembatan POC.
type BridgeOrder struct {
	OrderID       string `json:"order_id"`
	TotalReserved int64  `json:"total_reserved"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
	Status        string `json:"status"`
}

// ToOrderPending mengonversi BridgeOrder ke OrderPending.
func ToOrderPending(b BridgeOrder) types.OrderPending {
	return types.OrderPending{
		OrderID: b.OrderID, TotalSnapshot: b.TotalReserved,
		CreatedAt: b.CreatedAt, ExpiryAt: b.ExpiresAt,
		GraceEndAt: b.ExpiresAt, Status: b.Status,
	}
}

func joinBase(base string) string { return strings.TrimRight(base, "/") }

func parseBridgeTime(s string) bool {
	if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

func validBridgeOrder(b BridgeOrder) bool {
	return b.OrderID != "" && b.Status != "" && b.TotalReserved >= 0 &&
		parseBridgeTime(b.CreatedAt) && parseBridgeTime(b.ExpiresAt)
}

// FetchBridgeOrders mengambil daftar order dari mock jembatan POC.
// Token opsional (bila diisi, dikirim sebagai Bearer). Item yang
// tidak valid -> error (bukan dilewati diam-diam), sesuai kontrak POC.
func FetchBridgeOrders(ctx context.Context, client *http.Client, baseURL, token string) ([]BridgeOrder, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinBase(baseURL)+"/api/internal/pending-orders", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("mock backend menjawab HTTP %d untuk pending-orders", res.StatusCode)
	}
	var raw []map[string]any
	dec := json.NewDecoder(res.Body)
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("pending-orders mock bukan array JSON yang valid: %w", err)
	}
	orders := make([]BridgeOrder, 0, len(raw))
	for _, item := range raw {
		orderID, _ := item["order_id"].(string)
		created, _ := item["created_at"].(string)
		expires, _ := item["expires_at"].(string)
		status, _ := item["status"].(string)
		var total int64
		switch v := item["total_reserved"].(type) {
		case json.Number:
			n, err := v.Int64()
			if err != nil {
				return nil, errors.New("pending-orders mock berisi order tidak valid")
			}
			total = n
		default:
			return nil, errors.New("pending-orders mock berisi order tidak valid")
		}
		b := BridgeOrder{OrderID: orderID, TotalReserved: total, CreatedAt: created, ExpiresAt: expires, Status: status}
		if !validBridgeOrder(b) {
			return nil, errors.New("pending-orders mock berisi order tidak valid")
		}
		orders = append(orders, b)
	}
	return orders, nil
}

// PostProposal mengirim satu proposal ke mock jembatan POC.
// Mengembalikan true bila mock menjawab 2xx. Usulan BUKAN kredit.
func PostProposal(ctx context.Context, client *http.Client, baseURL string, proposal types.Proposal, token string) (bool, error) {
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(proposal)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinBase(baseURL)+"/api/internal/proposals", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	return res.StatusCode >= 200 && res.StatusCode < 300, nil
}
