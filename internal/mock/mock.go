// Package mock adalah sumber mutasi mock lokal.
//
//   - Data dibaca dari file JSON di DATA_DIR (env) atau dari folder
//     `data/` bila env tidak diisi.
//   - File yang dibaca: `orders.dummy.json` dan `mutations.dummy.json`.
//     Format file ini DIKUNCI sebagai berikut:
//     orders.dummy.json:    array JSON dari objek OrderPending
//     mutations.dummy.json: array JSON dari objek Mutasi
//     Format ini HANYA untuk mock lokal; ia bukan klaim format API
//     GoBiz asli dan bukan format endpoint internal backend.
//   - Loader ini sengaja TIDAK memvalidasi isi secara diam-diam: ia
//     mengembalikan array mentah apa adanya, dan validasi per item
//     dilakukan oleh match (input tidak valid -> entri ERROR, bukan
//     crash). Satu-satunya error yang dilempar loader adalah file
//     tidak ditemukan / JSON tidak bisa di-parse — itu dicatat
//     server sebagai "sumber-mock-error".
//   - Semua data di file dummy adalah DUMMY (TX-DUMMY-*, ORDER-DUMMY-*).
//     Jangan menaruh data asli di sini.
package mock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DataDir mengembalikan direktori data: env DATA_DIR, atau `data/`.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("DATA_DIR")); v != "" {
		return v
	}
	return "data"
}

// OrdersFilePath mengembalikan path file order dummy.
func OrdersFilePath(dir string) string { return filepath.Join(dir, "orders.dummy.json") }

// MutationsFilePath mengembalikan path file mutasi dummy.
func MutationsFilePath(dir string) string { return filepath.Join(dir, "mutations.dummy.json") }

// LoadJSONFile membaca dan mem-parse satu file JSON, mengembalikan
// nilai mentah. Melempar error bila file hilang atau JSON rusak.
func LoadJSONFile(path string) (any, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// LoadOrdersRaw memuat array mentah order dari file dummy.
func LoadOrdersRaw(dir string) ([]any, error) {
	raw, err := LoadJSONFile(OrdersFilePath(dir))
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("file order bukan array JSON: %s", OrdersFilePath(dir))
	}
	return arr, nil
}

// LoadMutationsRaw memuat array mentah mutasi dari file dummy.
func LoadMutationsRaw(dir string) ([]any, error) {
	raw, err := LoadJSONFile(MutationsFilePath(dir))
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("file mutasi bukan array JSON: %s", MutationsFilePath(dir))
	}
	return arr, nil
}
