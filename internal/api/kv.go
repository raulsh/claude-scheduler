package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// maxKVValue caps a stored value, matching the request-body limit decodeJSON
// already applies. The store is scratch space for runs, not a blob store.
const maxKVValue = 1 << 20

// maxKVKey bounds a key so the primary key index stays useful and a
// pathological path cannot be used to bloat the table.
const maxKVKey = 512

// kvListResponse is the metadata listing. Values are deliberately absent:
// listing a namespace should not transfer every byte it holds.
type kvListResponse struct {
	Entries []store.KVEntry `json:"entries"`
}

func (s *Server) handleListKV(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.ListKV(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		writeStoreError(w, err, "list kv")
		return
	}
	writeJSON(w, http.StatusOK, kvListResponse{Entries: orEmpty(entries)})
}

func (s *Server) handleGetKV(w http.ResponseWriter, r *http.Request) {
	key, ok := kvKey(w, r)
	if !ok {
		return
	}

	entry, err := s.store.GetKV(r.Context(), key)
	if err != nil {
		writeStoreError(w, err, "get kv")
		return
	}

	// The value goes back as the bytes that were stored, so that a file put
	// in comes out byte-identical and `kv get` can be piped anywhere.
	w.Header().Set("Content-Type", "application/octet-stream")
	// Set explicitly so a HEAD request, which GET patterns also match, is a
	// usable existence-and-size probe.
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	w.Header().Set("X-Kv-Updated-At", entry.UpdatedAt.UTC().Format(time.RFC3339))
	if entry.Expires() {
		w.Header().Set("X-Kv-Expires-At", entry.ExpiresAt.UTC().Format(time.RFC3339))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(entry.Value)
}

func (s *Server) handleSetKV(w http.ResponseWriter, r *http.Request) {
	key, ok := kvKey(w, r)
	if !ok {
		return
	}

	var expiresAt time.Time
	if raw := r.URL.Query().Get("ttl"); raw != "" {
		ttl, err := ParseTTL(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Zero means no expiry, matching executor.misfire_grace and the rest
		// of this config's "0 disables" convention. A negative TTL is an
		// error rather than an instant expiry: someone deriving it from a
		// deadline that has already passed wants to be told, not to have
		// the write succeed and vanish.
		if ttl > 0 {
			expiresAt = time.Now().Add(ttl)
		}
	}

	// The writer is passed to MaxBytesReader, unlike decodeJSON, so the
	// server can close the connection instead of leaving an abandoned
	// multi-megabyte body in the socket after the 413.
	value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxKVValue))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"value exceeds the %d byte limit; the kv store is for run state, not large files",
				tooLarge.Limit))
			return
		}
		writeError(w, http.StatusBadRequest, "read value: "+err.Error())
		return
	}

	if err := s.store.SetKV(r.Context(), key, value, expiresAt); err != nil {
		writeStoreError(w, err, "set value")
		return
	}

	// The resulting metadata comes back, as every other mutation in this API
	// returns what it produced, so `kv set --ttl 24h` can report the expiry
	// without a second round trip.
	writeJSON(w, http.StatusOK, store.KVEntry{
		Key:       key,
		Size:      int64(len(value)),
		ExpiresAt: expiresAt,
	})
}

func (s *Server) handleDeleteKV(w http.ResponseWriter, r *http.Request) {
	key, ok := kvKey(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteKV(r.Context(), key); err != nil {
		writeStoreError(w, err, "delete value")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ParseTTL reads a TTL, extending time.ParseDuration with a day unit.
//
// time.ParseDuration stops at hours, and "7d" is the obvious thing to type
// for a value meant to survive a week. Rejecting it and demanding "168h"
// would be consistent with config.Duration and unhelpful here, since a TTL
// is typed at a prompt rather than written once in a config file.
func ParseTTL(raw string) (time.Duration, error) {
	invalid := fmt.Errorf("ttl %q is not a duration; write it like 30m, 24h or 7d", raw)

	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, invalid
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}

	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, invalid
	}
	return ttl, nil
}

// kvKey reads and validates the {key...} path segment, which is a
// multi-segment wildcard so that a slash-separated key like
// "report/last-cursor" is one key rather than a nested route.
func kvKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("key")

	switch {
	case key == "":
		// Reachable: /api/v1/kv/ matches {key...} with an empty remainder.
		writeError(w, http.StatusBadRequest, "a key is required")
		return "", false
	case len(key) > maxKVKey:
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"key is %d bytes; the limit is %d", len(key), maxKVKey))
		return "", false
	}

	// A key travels as a URL path, so it has to survive being one. ServeMux
	// cleans the request path before matching and redirects when cleaning
	// changed it, and Go's HTTP client turns that 301 into a GET, so a key
	// with an empty, "." or ".." segment would quietly convert a PUT into a
	// successful-looking read and discard the write.
	for segment := range strings.SplitSeq(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			writeError(w, http.StatusBadRequest,
				`key segments cannot be empty, "." or "..": `+strconv.Quote(key))
			return "", false
		}
	}

	// Control characters and spaces are storable and miserable everywhere
	// else: in a URL, in a shell argument, in a log line. Restricting to
	// printable ASCII also sidesteps Unicode normalisation, where the NFC
	// and NFD spellings of one visible key become two distinct rows.
	for _, ch := range key {
		if ch <= ' ' || ch >= 0x7f {
			writeError(w, http.StatusBadRequest,
				"key may only contain printable ASCII without spaces: "+strconv.Quote(key))
			return "", false
		}
	}
	return key, true
}
