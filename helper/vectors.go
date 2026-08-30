package main

// docs/<id>.vec — the embedding cache for one document.
//
// The lexical index has no file on purpose: it rebuilds from the documents in
// milliseconds, so a format that could be stale or half-written buys nothing.
// Embeddings are the opposite. Encoding a few thousand passages takes minutes
// of real compute, and doing it on every launch would make the drive unusable
// on the machine you just plugged it into.
//
// So this file exists, but it is DERIVED, and it is written to be thrown away:
//
//   - Content-addressed. Each vector is keyed by a hash of the passage text,
//     not by its position, so re-adding a file, re-chunking, or changing the
//     order of documents never mismatches a vector to the wrong passage.
//   - Self-invalidating. The header records the embedding model and dimension.
//     Swap the model on the drive and every cache is discarded on read rather
//     than mixing two incompatible vector spaces into one ranking.
//   - Torn writes are free. The payload length must be exactly count × record
//     size; if it is not, the file is dropped and the vectors are recomputed.
//     Chat transcripts are append-only because losing one costs the user their
//     conversation. Losing this costs a few minutes of background work.
//
// Delete every .vec on the drive and the only consequence is that the next
// launch is busy for a while.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// 16 bytes of sha256. The vectors themselves are 1-3KB each, so a wider key
// costs nothing measurable and takes collisions off the table entirely.
type chunkHash [16]byte

func hashChunk(text string) chunkHash {
	sum := sha256.Sum256([]byte(text))
	var h chunkHash
	copy(h[:], sum[:16])
	return h
}

type vecHeader struct {
	DocID   string `json:"docId"`
	Model   string `json:"model"`
	Dim     int    `json:"dim"`
	Count   int    `json:"count"`
	Written string `json:"written"`
}

type VecStore struct {
	dir string
}

func NewVecStore(dir string) *VecStore { return &VecStore{dir: dir} }

func (v *VecStore) path(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", errors.New("bad document id")
	}
	return filepath.Join(v.dir, id+".vec"), nil
}

// Load returns the cached vectors for one document, or an empty map for any
// reason at all. Every failure mode here — absent, truncated, written by a
// different model, corrupt header — has the same correct response: treat it as
// a cache miss and let the vectors be computed again.
func (v *VecStore) Load(id, model string) map[chunkHash][]float32 {
	empty := map[chunkHash][]float32{}

	p, err := v.path(id)
	if err != nil {
		return empty
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return empty
	}
	nl := bytes.IndexByte(raw, '\n')
	if nl < 0 {
		return empty
	}
	var h vecHeader
	if err := json.Unmarshal(raw[:nl], &h); err != nil {
		return empty
	}
	if h.Model != model || h.Dim <= 0 || h.Count < 0 {
		return empty
	}

	body := raw[nl+1:]
	rec := len(chunkHash{}) + h.Dim*4
	if rec <= 0 || len(body) != h.Count*rec {
		// Exactly the shape a yanked drive leaves behind.
		return empty
	}

	out := make(map[chunkHash][]float32, h.Count)
	for i := 0; i < h.Count; i++ {
		off := i * rec
		var key chunkHash
		copy(key[:], body[off:off+len(key)])
		vec := make([]float32, h.Dim)
		for d := 0; d < h.Dim; d++ {
			bits := binary.LittleEndian.Uint32(body[off+len(key)+d*4:])
			f := math.Float32frombits(bits)
			// A NaN would poison every comparison it takes part in, and
			// silently: NaN sorts as neither greater nor less.
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				return empty
			}
			vec[d] = f
		}
		out[key] = vec
	}
	return out
}

// Save writes the vectors for one document. Written to a temporary name and
// renamed, so a reader never sees a half-built file — and if the rename itself
// is interrupted, the length check on read catches it.
func (v *VecStore) Save(id, model string, dim int, vecs map[chunkHash][]float32) error {
	p, err := v.path(id)
	if err != nil {
		return err
	}
	for _, vec := range vecs {
		if len(vec) != dim {
			return fmt.Errorf("vector of length %d in a %d-dimensional cache", len(vec), dim)
		}
	}

	head, err := json.Marshal(vecHeader{
		DocID: id, Model: model, Dim: dim, Count: len(vecs), Written: nowRFC3339(),
	})
	if err != nil {
		return err
	}
	rec := len(chunkHash{}) + dim*4
	buf := make([]byte, 0, len(head)+1+len(vecs)*rec)
	buf = append(buf, head...)
	buf = append(buf, '\n')
	for key, vec := range vecs {
		buf = append(buf, key[:]...)
		for _, f := range vec {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(f))
		}
	}

	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (v *VecStore) Delete(id string) {
	if p, err := v.path(id); err == nil {
		os.Remove(p)
		os.Remove(p + ".tmp")
	}
}

// Wipe removes every cache file. Called when the documents themselves are
// erased: a vector is a lossy but real representation of the text it came from,
// so leaving them behind would leave part of the document behind.
func (v *VecStore) Wipe() int {
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".vec") || strings.HasSuffix(name, ".vec.tmp")) {
			continue
		}
		if err := os.Remove(filepath.Join(v.dir, name)); err == nil {
			n++
		}
	}
	return n
}

// Sweep drops caches for documents that no longer exist. Deleting a document
// removes its .vec directly; this catches the file removed from docs/ by hand,
// which is a thing people do with a USB stick.
func (v *VecStore) Sweep(live map[string]bool) {
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".vec") {
			continue
		}
		if id := strings.TrimSuffix(name, ".vec"); !live[id] {
			os.Remove(filepath.Join(v.dir, name))
		}
	}
}
