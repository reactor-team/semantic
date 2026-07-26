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

type localInferencer struct {
	session    *ort.DynamicAdvancedSession
	tok        *tokenizer.Tokenizer
	useTypeIDs bool   // whether the model accepts token_type_ids
	model      *Model // the checkpoint this session was built from
}

var (
	// initMu serializes init and guards inferencer. We deliberately do NOT
	// use sync.Once here — a failed init (model not yet on disk, ORT runtime
	// missing) must be retryable so a caller that runs `init` and retries in
	// the same process can recover instead of caching the failure for the
	// process lifetime.
	initMu     sync.Mutex
	inferencer *localInferencer

	// ortReady records that the ORT environment is up. Initializing it is a
	// process-global, once-only act, so it cannot live with the session, which
	// is torn down and rebuilt whenever the selected model changes.
	ortReady bool

	// runMu serializes embed() calls: ORT sessions aren't safe for
	// concurrent Run(), and the CLI embeds serially, so one shared session
	// behind a mutex is sufficient (and far simpler than a session pool).
	runMu sync.Mutex
)

// getInferencer lazily builds the shared session, initializing the ORT
// runtime on first use. Safe to call repeatedly; retries after a failed init.
//
// A session built for a different checkpoint than the one now selected is
// discarded rather than reused. Select already tears one down, so this is the
// backstop for a caller that changed the selection some other way — embedding
// with the wrong weights would produce vectors nothing downstream could tell
// apart from the right ones.
func getInferencer() (*localInferencer, error) {
	initMu.Lock()
	defer initMu.Unlock()
	m := Current()
	if inferencer != nil {
		if inferencer.model == m {
			return inferencer, nil
		}
		inferencer.close()
		inferencer = nil
	}
	if err := Check(); err != nil {
		return nil, err
	}

	if !ortReady {
		ort.SetSharedLibraryPath(findOrtLib())
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("ONNX Runtime init: %w", err)
		}
		ortReady = true
	}

	modelDir := ModelCacheDir()
	inf, err := newInferencer(filepath.Join(modelDir, "model.onnx"), filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	inf.model = m
	inferencer = inf
	return inferencer, nil
}

// resetInferencer drops the live session so the next embed builds one against
// whatever checkpoint is now selected. The ORT environment stays up: it is
// process-global and not tied to any one model.
func resetInferencer() {
	initMu.Lock()
	defer initMu.Unlock()
	if inferencer != nil {
		inferencer.close()
		inferencer = nil
	}
}

// close releases the ORT session. Called when the selection changes, which in
// a long-lived process would otherwise leak one native session per switch.
func (l *localInferencer) close() {
	if l.session != nil {
		l.session.Destroy()
		l.session = nil
	}
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

	// Ask the graph itself which inputs and outputs it declares. A dynamic
	// session accepts any name list at construction and only fails at Run(),
	// so probing by attempting construction and catching the error (the prior
	// approach for token_type_ids) misses models that build fine but reject
	// the tensor on the first embed — found on mxbai-embed-xsmall-v1, whose
	// graph has neither a token_type_ids input nor a last_hidden_state output.
	inputs, outputs, err := ort.GetInputOutputInfoWithONNXData(modelBytes)
	if err != nil {
		return nil, fmt.Errorf("reading model inputs: %w", err)
	}
	useTypeIDs := false
	names := []string{"input_ids", "attention_mask"}
	for _, in := range inputs {
		if in.Name == "token_type_ids" {
			useTypeIDs = true
			names = append(names, "token_type_ids")
			break
		}
	}

	// The per-token hidden state is exported under one of two names depending
	// on the tool that produced the ONNX file. Either shape is [1, seqLen,
	// Dim] and pool() treats them identically — the pooling choice comes from
	// Model.Pooling, not from whichever the export also happened to bake in,
	// so a checkpoint scores the same regardless of which name it used.
	hiddenName := "last_hidden_state"
	for _, out := range outputs {
		if out.Name == "token_embeddings" {
			hiddenName = "token_embeddings"
			break
		}
	}

	sess, err := ort.NewDynamicAdvancedSessionWithONNXData(modelBytes, names, []string{hiddenName}, opts)
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

	// Truncate to the checkpoint's window.
	seqLen := len(rawIDs)
	if seqLen > l.model.MaxSeqLen {
		seqLen = l.model.MaxSeqLen
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
	// the per-token hidden state: [1, seqLen, Dim] — flat []float32 of length seqLen*Dim
	hidden := hiddenTensor.GetData()
	result := pool(l.model.Pooling, hidden, mask, seqLen, l.model.Dim)
	hiddenTensor.Destroy()

	normalizeVec(result)
	return Vec(result), nil
}

// pool reduces the per-token hidden states to one vector the way the
// checkpoint was trained to be reduced. The two modes are not interchangeable:
// applying the wrong one yields a coherent-looking vector that ranks badly,
// with nothing downstream able to tell, which is why the mode is a field on
// Model and part of RepresentationID rather than a global choice.
//
// An unrecognized mode falls back to CLS instead of failing. Pooling sits in
// the middle of indexing a repository, and Model values come from a registry
// this package controls, so the unreachable case degrades rather than aborts.
func pool(mode Pooling, hidden []float32, mask []int64, seqLen, dim int) []float32 {
	if mode == PoolMean {
		return meanPool(hidden, mask, seqLen, dim)
	}
	return clsPool(hidden, dim)
}

// clsPool takes the first token's hidden state as the sentence embedding. The
// [CLS] position is where a BGE-style training objective puts the sentence
// representation.
//
// A short output is padded rather than treated as an error. hidden crosses the
// ONNX boundary, and a model whose output shape differs from the one assumed
// here should degrade rather than panic in the middle of indexing a repository.
func clsPool(hidden []float32, dim int) []float32 {
	out := make([]float32, dim)
	if dim <= 0 {
		return out
	}
	copy(out, hidden[:min(dim, len(hidden))])
	return out
}

// meanPool averages over the real tokens only, which is what the
// sentence-transformers MiniLM checkpoints are trained with. The padding a
// batch adds carries no meaning, and letting it into the average would pull
// every short chunk's vector toward the same point.
//
// Short inputs are handled the same way clsPool handles them: whatever is
// actually present is averaged, and a fully masked input returns zeros rather
// than dividing by zero and poisoning every later cosine comparison with NaN.
func meanPool(hidden []float32, mask []int64, seqLen, dim int) []float32 {
	out := make([]float32, dim)
	if dim <= 0 {
		return out
	}
	var count int
	for t := 0; t < seqLen && t < len(mask); t++ {
		if mask[t] == 0 {
			continue
		}
		base := t * dim
		if base+dim > len(hidden) {
			break
		}
		for d := range dim {
			out[d] += hidden[base+d]
		}
		count++
	}
	if count == 0 {
		return out
	}
	for d := range out {
		out[d] /= float32(count)
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
