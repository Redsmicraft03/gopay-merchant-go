// Package config membaca konfigurasi dari environment. File .env
// di folder kerja dimuat oleh masing-masing binary (cmd/server,
// cmd/wire, cmd/login) lewat godotenv sebelum config dibaca;
// env yang sudah di-set manual tetap menang.
//
// Nilai default dipertahankan dari versi TypeScript, kecuali:
//   - POLLER_DB_PATH (SQLite TS) DIHAPUS, diganti DATABASE_URL
//     (Postgres GORM). Lihat README bagian "Yang berubah dari TS".
//   - POLLER_INTERNAL_TOKEN tetap untuk POST /mdr/reserve.
//   - BRIDGE_INTERNAL_TOKEN tetap untuk wire (bukan di sini, dibaca
//     cmd/wire langsung dari env).
package config

import (
	"os"
	"strconv"
	"strings"

	"gopay-merchant-go/internal/gobiz"
	"gopay-merchant-go/internal/mock"
)

// ServerConfig adalah konfigurasi server poller Fiber.
type ServerConfig struct {
	Port                int
	PollIntervalMs      int64
	DataDir             string
	PollEnabled         bool
	Source              string // Nilai mentah SOURCE (divalidasi di server).
	Gobiz               gobiz.Config
	PollerInternalToken string
	MDRDurationMs       int64
	DatabaseURL         string
	UIPath              string
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// LoadServerConfig membaca konfigurasi server dari env.
func LoadServerConfig() ServerConfig {
	return ServerConfig{
		Port:                int(envInt("PORT", 3101)),
		PollIntervalMs:      envInt("POLL_INTERVAL_MS", 60_000),
		DataDir:             mock.DataDir(),
		PollEnabled:         strings.TrimSpace(os.Getenv("POLL_ENABLED")) == "true",
		Source:              os.Getenv("SOURCE"),
		Gobiz:               gobiz.ConfigFromEnv(),
		PollerInternalToken: os.Getenv("POLLER_INTERNAL_TOKEN"),
		MDRDurationMs:       envInt("POLLER_MDR_DURATION_MS", 900_000),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		UIPath:              envOr("UI_PATH", "web/ui.html"),
	}
}

// SessionFileExists mengembalikan true bila file sesi GoBiz ada.
func SessionFileExists(cfg gobiz.Config) bool {
	_, err := os.Stat(cfg.SessionFile)
	return err == nil
}
