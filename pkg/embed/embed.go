// Package embed generates sentence embeddings with a local ONNX model
// (arctic-embed-xs by default; see models.go for the registry). Inference runs fully in-process against a
// single ONNX Runtime session — no network, no API key. The runtime
// library and model files are downloaded once (see download.go) and
// cached under the OS cache dir (see paths.go).
package embed

import "math"

// Vec is a float32 embedding vector.
type Vec []float32

// Get returns a normalized embedding vector for text using the local
// ONNX model. Returns an error if the model is not installed — run the
// binary's `init` command to download it (~160 MB).
//
// v1 has no warm daemon: every process pays the ONNX session cold-start
// (~800ms) on its first Get. The session then stays warm for the life of
// the process, so batch indexing amortizes the cost.
func Get(text string) (Vec, error) {
	return embedPrefixed(Current().DocPrefix, text)
}

// embedPrefixed is the one path to the model. Get and GetQuery differ only in
// which marker they put in front, and neither may see the other's — a query
// carrying the passage marker as well is the exact failure the two fields
// exist to prevent.
func embedPrefixed(prefix, text string) (Vec, error) {
	inf, err := getInferencer()
	if err != nil {
		return nil, err
	}
	runMu.Lock()
	defer runMu.Unlock()
	return inf.embed(prefix + text)
}

// GetQuery embeds text as a search query. Use it for what the user typed; use
// Get for the content being searched.
//
// An asymmetric checkpoint is trained to see a marker in front of a query and
// nothing in front of the passages it ranks, because a short question and the
// long passage answering it are not the same kind of text. Model.QueryPrefix
// carries whichever marker the selected checkpoint wants, and is empty for a
// symmetric one, which makes this Get.
//
// The prefix touches the query alone, so stored vectors are unaffected and
// RepresentationID does not name it — an index built before this existed stays
// valid, and no reindex is needed to benefit.
//
// Symmetric comparisons — `dupes`, where both sides are passages from the
// corpus — deliberately do not use this. There is no query in that pairing,
// and prefixing one arbitrary side would tilt it. They call Get, which is also
// what gives both sides the document marker a checkpoint like E5 expects.
func GetQuery(text string) (Vec, error) {
	return embedPrefixed(Current().QueryPrefix, text)
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
