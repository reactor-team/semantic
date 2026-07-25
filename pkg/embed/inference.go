// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
	ort "github.com/yalue/onnxruntime_go"
)

const (
	modelDim = 384

	// maxSeqLen caps tokens per embed. all-MiniLM-L6-v2 was trained at
	// 256. Markdown sections run long, so we keep the model's full window.
	// Chunks past this are truncated (the chunker aims to stay under it).
	maxSeqLen = 256
)

type localInferencer struct {
	session    *ort.DynamicAdvancedSession
	tok        *tokenizer.Tokenizer
	useTypeIDs bool // whether the model accepts token_type_ids
}

var (
	// initMu serializes init and guards inferencer. We deliberately do NOT
	// use sync.Once here — a failed init (model not yet on disk, ORT runtime
	// missing) must be retryable so a caller that runs `init` and retries in
	// the same process can recover instead of caching the failure for the
	// process lifetime.
	initMu     sync.Mutex
	inferencer *localInferencer

	// runMu serializes embed() calls: ORT sessions aren't safe for
	// concurrent Run(), and the CLI embeds serially, so one shared session
	// behind a mutex is sufficient (and far simpler than a session pool).
	runMu sync.Mutex
)

// getInferencer lazily builds the shared session, initializing the ORT
// runtime on first use. Safe to call repeatedly; retries after a failed init.
func getInferencer() (*localInferencer, error) {
	initMu.Lock()
	defer initMu.Unlock()
	if inferencer != nil {
		return inferencer, nil
	}
	if err := Check(); err != nil {
		return nil, err
	}

	ort.SetSharedLibraryPath(findOrtLib())
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("ONNX Runtime init: %w", err)
	}

	modelDir := ModelCacheDir()
	inf, err := newInferencer(filepath.Join(modelDir, "model.onnx"), filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	inferencer = inf
	return inferencer, nil
}

// newInferencer builds the ORT session + tokenizer pair.
//
// The session is pinned to 1 intra-op + 1 inter-op thread. Single-threaded
// inference keeps embedding results bit-for-bit reproducible (multi-threaded
// matmul reductions vary at float noise level, which would perturb search
// scores run to run); the CLI never fans embedding out across cores anyway.
func newInferencer(modelPath, tokPath string) (*localInferencer, error) {
	modelBytes, err := os.ReadFile(modelPath) //nolint:gosec // G304: modelPath is ModelCacheDir()-derived, overridable only via $SEMANTIC_MODEL_DIR (trusted local env), not user input
	if err != nil {
		return nil, fmt.Errorf("reading model: %w", err)
	}

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("creating session options: %w", err)
	}
	defer opts.Destroy()
	if err := opts.SetIntraOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("SetIntraOpNumThreads: %w", err)
	}
	if err := opts.SetInterOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("SetInterOpNumThreads: %w", err)
	}

	// Try with three inputs first; some exported models omit token_type_ids.
	outputNames := []string{"last_hidden_state"}
	sess, err := ort.NewDynamicAdvancedSessionWithONNXData(
		modelBytes,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		outputNames, opts,
	)
	useTypeIDs := true
	if err != nil {
		sess, err = ort.NewDynamicAdvancedSessionWithONNXData(
			modelBytes,
			[]string{"input_ids", "attention_mask"},
			outputNames, opts,
		)
		useTypeIDs = false
	}
	if err != nil {
		return nil, fmt.Errorf("creating ONNX session: %w", err)
	}

	tok, err := pretrained.FromFile(tokPath)
	if err != nil {
		return nil, fmt.Errorf("loading tokenizer: %w", err)
	}

	return &localInferencer{session: sess, tok: tok, useTypeIDs: useTypeIDs}, nil
}

func (l *localInferencer) embed(text string) (Vec, error) {
	enc, err := l.tok.EncodeSingle(text, true)
	if err != nil {
		return nil, fmt.Errorf("tokenizing: %w", err)
	}

	rawIDs := enc.GetIds()
	rawMask := enc.GetAttentionMask()
	rawTypeIDs := enc.GetTypeIds()

	// Truncate to maxSeqLen.
	seqLen := len(rawIDs)
	if seqLen > maxSeqLen {
		seqLen = maxSeqLen
		rawIDs = rawIDs[:seqLen]
		rawMask = rawMask[:seqLen]
		rawTypeIDs = rawTypeIDs[:seqLen]
	}

	// Convert []int → []int64 (ONNX Runtime requires int64 for token tensors).
	ids := make([]int64, seqLen)
	mask := make([]int64, seqLen)
	typeIDs := make([]int64, seqLen)
	for i := range rawIDs {
		ids[i] = int64(rawIDs[i])
		mask[i] = int64(rawMask[i])
		typeIDs[i] = int64(rawTypeIDs[i])
	}

	shape := ort.NewShape(1, int64(seqLen))

	inputIDsTensor, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, fmt.Errorf("input_ids tensor: %w", err)
	}
	defer inputIDsTensor.Destroy()

	maskTensor, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, fmt.Errorf("attention_mask tensor: %w", err)
	}
	defer maskTensor.Destroy()

	var ortInputs []ort.Value
	if l.useTypeIDs {
		typeIDsTensor, err := ort.NewTensor(shape, typeIDs)
		if err != nil {
			return nil, fmt.Errorf("token_type_ids tensor: %w", err)
		}
		defer typeIDsTensor.Destroy()
		ortInputs = []ort.Value{inputIDsTensor, maskTensor, typeIDsTensor}
	} else {
		ortInputs = []ort.Value{inputIDsTensor, maskTensor}
	}

	outputs := make([]ort.Value, 1)
	if err := l.session.Run(ortInputs, outputs); err != nil {
		return nil, fmt.Errorf("ONNX inference: %w", err)
	}

	hiddenTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		outputs[0].Destroy()
		return nil, fmt.Errorf("unexpected output type from ONNX model")
	}
	// last_hidden_state: [1, seqLen, modelDim] — flat []float32 of length seqLen*modelDim
	hidden := hiddenTensor.GetData()
	result := meanPool(hidden, mask, seqLen, modelDim)
	hiddenTensor.Destroy()

	normalizeVec(result)
	return Vec(result), nil
}

// meanPool averages token embeddings weighted by the attention mask.
//
// seqLen is clamped to what the two slices actually hold. embed builds both
// from the same length, so the clamp never fires there; it exists because
// hidden comes back across the ONNX boundary, and a model whose output shape
// differs from the one assumed here should degrade rather than panic in the
// middle of indexing a repository.
func meanPool(hidden []float32, mask []int64, seqLen, dim int) []float32 {
	out := make([]float32, dim)
	if dim <= 0 {
		return out
	}
	seqLen = max(0, min(seqLen, len(mask), len(hidden)/dim))

	var count float32
	for i, m := range mask[:seqLen] {
		if m == 0 {
			continue
		}
		count++
		for j, h := range hidden[i*dim : i*dim+dim] {
			out[j] += h
		}
	}
	if count > 0 {
		for j := range out {
			out[j] /= count
		}
	}
	return out
}

// normalizeVec performs L2 normalization in-place.
func normalizeVec(v []float32) {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / norm)
		}
	}
}
