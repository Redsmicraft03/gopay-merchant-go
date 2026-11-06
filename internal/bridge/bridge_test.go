package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mitrain-gopay/internal/types"
)

func TestKonversiBridgeOrder(t *testing.T) {
	b := BridgeOrder{
		OrderID: "ORDER-POC-001", TotalReserved: 101005,
		CreatedAt: "2026-10-28T10:00:00+07:00", ExpiresAt: "2026-10-28T10:15:00+07:00", Status: "PENDING",
	}
	o := ToOrderPending(b)
	if o.TotalSnapshot != 101005 || o.ExpiryAt != b.ExpiresAt || o.GraceEndAt != b.ExpiresAt {
		t.Fatalf("konversi = %+v, mau total_snapshot 101005 expiry=grace=expires_at", o)
	}
}

func TestFetchDanPostProposalMock(t *testing.T) {
	var posted types.Proposal
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/pending-orders", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TOKEN-POC" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"order_id":"ORDER-POC-001","total_reserved":101005,"created_at":"2026-10-28T10:00:00+07:00","expires_at":"2026-10-28T10:15:00+07:00","status":"PENDING"}]`))
	})
	mux.HandleFunc("/api/internal/proposals", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&posted)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	orders, err := FetchBridgeOrders(ctx, nil, srv.URL, "TOKEN-POC")
	if err != nil || len(orders) != 1 || orders[0].OrderID != "ORDER-POC-001" {
		t.Fatalf("orders = %+v err = %v", orders, err)
	}
	ok, err := PostProposal(ctx, nil, srv.URL, types.Proposal{OrderID: "ORDER-POC-001", TransactionID: "TX-001", Amount: 101005, TransactedAt: "2026-10-28T10:10:00+07:00", ProposalStatus: "DIUSULKAN"}, "TOKEN-POC")
	if err != nil || !ok || posted.OrderID != "ORDER-POC-001" {
		t.Fatalf("post = %v err = %v posted = %+v", ok, err, posted)
	}
	if _, err := FetchBridgeOrders(ctx, nil, srv.URL, "TOKEN-SALAH"); err == nil {
		t.Fatal("token salah harus error")
	}
}

func TestFetchBridgeOrderTidakValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"order_id":"ORDER-BAD","total_reserved":"bukan-angka","created_at":"2026-10-28T10:00:00+07:00","expires_at":"2026-10-28T10:15:00+07:00","status":"PENDING"}]`))
	}))
	defer srv.Close()
	if _, err := FetchBridgeOrders(context.Background(), nil, srv.URL, ""); err == nil {
		t.Fatal("order mock tidak valid harus error, bukan dilewati diam-diam")
	}
}
