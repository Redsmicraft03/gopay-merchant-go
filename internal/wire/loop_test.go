package wire

import (
	"context"
	"testing"
	"time"
)

// RunLoop dengan ctx yang sudah dibatalkan harus kembali segera
// tanpa menjalankan siklus apa pun (cek ctx di awal loop).
func TestRunLoopCtxDibatalkanKembaliSegera(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, LoopInput{BackendBaseURL: "http://127.0.0.1:1", Token: "TOKEN-TEST", BaseMs: 3000, MaxIdleMs: 300000})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop dengan ctx dibatalkan tidak kembali dalam 2 detik")
	}
}
