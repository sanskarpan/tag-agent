package memory

import "context"

// Exported thin wrappers over the package-internal vector primitives so other
// packages (notably internal/toolindex, PRD-043) can reuse the SAME float32 BLOB
// layout and cosine implementation instead of shipping a second, subtly
// different copy. Keeping one encoder means a vector written by one subsystem is
// always readable by another.

// EncodeVector packs a float32 vector into the little-endian BLOB layout used by
// semantic_memories.embedding.
func EncodeVector(v []float32) []byte { return encodeVector(v) }

// DecodeVector reverses EncodeVector, rejecting blobs whose length is not a
// multiple of 4 rather than silently truncating.
func DecodeVector(b []byte) ([]float32, error) { return decodeVector(b) }

// Cosine returns the cosine similarity of a and b in [-1,1]; 0 for mismatched
// lengths or a zero-norm operand.
func Cosine(a, b []float32) float64 { return cosine(a, b) }

// EmbedAll embeds inputs in fixed-size batches, preserving order, so large
// corpora don't exceed the embeddings API's array/token/response limits.
func EmbedAll(ctx context.Context, e Embedder, inputs []string) ([][]float32, error) {
	return embedAll(ctx, e, inputs)
}
