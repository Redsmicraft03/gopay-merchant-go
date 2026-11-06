package wirelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mitrain-gopay/internal/types"
)

func TestAppendWireLogSatuBarisJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wire-decisions-test.log")
	entry := Entry{
		Timestamp:    "2026-10-28T10:10:00+07:00",
		SettledCount: 0, SettledIDs: []string{}, Rejected: []RejectedEntry{},
		Skipped: false, HadError: false,
		Decisions: DecisionLogEntries([]types.AuditLogEntry{{
			LoggedAt: "2026-10-28T10:10:00+07:00",
			OrderID:  types.StrPtr("ORDER-001"), TransactionID: types.StrPtr("TX-001"),
			Decision: types.DecisionNotProposed, Reason: types.ReasonAmountMismatch,
			Amount: types.IntPtr(1000), TransactedAt: types.StrPtr("2026-10-28T10:10:00+07:00"),
			MDROrder: types.IntPtr(1216), MDRTersirat: types.IntPtr(0), MDRCocok: types.BoolPtr(false),
		}}),
	}
	AppendWireLog(entry, path)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("baca log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 1 {
		t.Fatalf("baris log = %d, mau 1", len(lines))
	}
	var parsed Entry
	if err := json.Unmarshal([]byte(lines[0]), &parsed); err != nil {
		t.Fatalf("log bukan JSON: %v", err)
	}
	if len(parsed.Decisions) != 1 || parsed.Decisions[0].MDROrder == nil || *parsed.Decisions[0].MDROrder != 1216 {
		t.Fatalf("decisions log = %+v, mau mdr_order 1216", parsed.Decisions)
	}
	if strings.Contains(string(content), "BRIDGE_INTERNAL_TOKEN") || strings.Contains(string(content), "Bearer") || strings.Contains(string(content), "access_token") {
		t.Fatal("log tidak boleh mengandung token/sesi")
	}
}

func TestAppendWireLogGagalTidakPanic(t *testing.T) {
	// Path adalah direktori yang sudah ada: OpenFile gagal, tapi
	// AppendWireLog tidak boleh panic dan tidak boleh menggagalkan siklus.
	dir := t.TempDir()
	AppendWireLog(Entry{Timestamp: "2026-10-28T10:10:00+07:00"}, dir)
}
