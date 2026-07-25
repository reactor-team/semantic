// Package embed generates sentence embeddings with a local ONNX model
// (all-MiniLM-L6-v2, 384-dim). Inference runs fully in-process against a
// single ONNX Runtime session — no network, no API key. The runtime
// library and model files are downloaded once (see download.go) and
// cached under the OS cache dir (see paths.go).
package embed

import "math"

// Vec is a float32 embedding vector.
type Vec []float32

// Get returns a normalized embedding vector for text using the local
// ONNX model. Returns an error if the model is not installed — run the
// binary's `init` command to download it (~23 MB).
//
// v1 has no warm daemon: every process pays the ONNX session cold-start
// (~800ms) on its first Get. The session then stays warm for the life of
// the process, so batch indexing amortizes the cost.
func Get(text string) (Vec, error) {
	inf, err := getInferencer()
	if err != nil {
		return nil, err
	}
	runMu.Lock()
	defer runMu.Unlock()
	return inf.embed(text)
}

// CosineSim returns cosine similarity in [-1, 1]. Vectors from Get are
// already L2-normalized, so for those this reduces to the dot product,
// but the full formula is kept so callers can pass un-normalized inputs.
func CosineSim(a, b Vec) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, nA, nB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		nA += ai * ai
		nB += bi * bi
	}
	if nA == 0 || nB == 0 {
		return 0
	}
	return dot / (math.Sqrt(nA) * math.Sqrt(nB))
}
