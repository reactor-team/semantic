package embed

import (
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

// Pooling names how a model reduces its per-token hidden states to one vector.
// It is a property of the checkpoint, not a preference: a model is trained
// against one of these, and using the other yields a coherent-looking vector
// that ranks badly, with no error to notice.
type Pooling string

const (
	// PoolCLS takes the first token. Models trained with a [CLS] objective
	// (the BGE family) put the sentence representation there.
	PoolCLS Pooling = "cls"
	// PoolMean averages the unmasked tokens. What the sentence-transformers
	// MiniLM checkpoints are trained with.
	PoolMean Pooling = "mean"
)

// Model is one embedding checkpoint and everything that makes its vectors what
// they are. Two indexes built with different Models hold cosine-incomparable
// vectors even at the same dimension, which is why every field here feeds
// RepresentationID.
type Model struct {
	// Name is the checkpoint's identity, as the user types it and as the
	// index records it. It also names the cache directory, so two models
	// coexist on disk rather than overwriting each other's weights.
	Name string

	// Dim is the width of the output vector.
	Dim int

	// MaxSeqLen caps tokens per embed. Raising it changes the vector for every
	// chunk long enough to have been truncated at the old cap, so it is pinned
	// per checkpoint rather than shared.
	MaxSeqLen int

	// Pooling is how the token states collapse to one vector.
	Pooling Pooling

	// QueryPrefix is prepended by GetQuery and by nothing else. Asymmetric
	// models are trained to see a marker on the query side so they can tell a
	// short question from the long passage answering it; a symmetric model
	// leaves this empty and GetQuery becomes Get.
	QueryPrefix string

	// DocPrefix is prepended by Get, so it marks everything that goes into the
	// index. Most asymmetric checkpoints mark only the query and leave this
	// empty; the E5 family marks both sides and ranks badly if either marker
	// is missing. Unlike QueryPrefix this rewrites the stored vectors, so
	// RepresentationID names it.
	DocPrefix string

	// ModelURL and TokenizerURL are where `semantic init` fetches the files.
	ModelURL     string
	TokenizerURL string

	// ApproxMB is the model download's rough size, for the progress line. It
	// is what the user is about to spend, so it is worth saying before it is
	// spent.
	ApproxMB int
}

// RepresentationID names the vector space this model produces. The index
// stores it and rebuilds itself when it stops matching, so anything that
// changes what Get returns for the same input must be reflected here.
//
// Every component is load-bearing:
//
//   - the checkpoint, because different weights mean different vectors;
//   - the pooling and normalization, because mean-vs-CLS pooling or dropping
//     the L2 norm rewrites the space without changing its dimension;
//   - the dimension, which is the one mismatch that would fail loudly anyway;
//   - the sequence cap, because raising it changes the vector for every chunk
//     long enough to have been truncated at the old cap — silently, and only
//     for the long chunks, which is the worst kind of drift to debug.
//
// It is derived from the fields it names rather than written out, so it cannot
// drift from them. QueryPrefix is deliberately absent: it touches the query
// alone and leaves every stored vector unchanged, so an index built before a
// prefix existed stays valid.
//
// DocPrefix is the opposite case and does appear, because it is embedded into
// every stored vector. It appears only when set, so the checkpoints that
// predate the field keep the IDs they already stamped into existing indexes.
func (m *Model) RepresentationID() string {
	id := fmt.Sprintf("%s+%s+l2+d%d+s%d", m.Name, m.Pooling, m.Dim, m.MaxSeqLen)
	if m.DocPrefix != "" {
		h := fnv.New32a()
		_, _ = io.WriteString(h, m.DocPrefix)
		id += fmt.Sprintf("+dp%08x", h.Sum32())
	}
	return id
}

// DefaultModel is what a caller who expresses no preference gets.
const DefaultModel = "arctic-embed-xs"

// registry is the set of checkpoints this build knows how to fetch and run.
// Membership is a support commitment — each entry's pooling and prefix have to
// be right, and neither is verifiable from the file it downloads — so it is a
// curated list rather than an arbitrary Hugging Face path.
var registry = map[string]*Model{
	"arctic-embed-xs": { //nolint:gosec // G101 false positive: the URLs below are public asset paths, not credentials
		// Measured ahead of bge-small-en-v1.5 on a held-out retrieval
		// benchmark, at roughly two-thirds the download. It shares bge's CLS
		// pooling and query marker, so switching cost is a smaller download
		// rather than a different retrieval strategy.
		Name:      "arctic-embed-xs",
		Dim:       384,
		MaxSeqLen: 512,
		Pooling:   PoolCLS,
		// Same marker as the BGE family — Snowflake trained this checkpoint
		// on the same asymmetric-retrieval convention.
		QueryPrefix:  "Represent this sentence for searching relevant passages: ",
		ModelURL:     "https://huggingface.co/Snowflake/snowflake-arctic-embed-xs/resolve/main/onnx/model.onnx",
		TokenizerURL: "https://huggingface.co/Snowflake/snowflake-arctic-embed-xs/resolve/main/tokenizer.json",
		ApproxMB:     90,
	},
	"bge-small-en-v1.5": { //nolint:gosec // G101 false positive: the URLs below are public asset paths, not credentials
		Name:      "bge-small-en-v1.5",
		Dim:       384,
		MaxSeqLen: 512,
		Pooling:   PoolCLS,
		// The BGE family is trained for asymmetric retrieval and ships this
		// exact sentence as the query-side marker.
		QueryPrefix:  "Represent this sentence for searching relevant passages: ",
		ModelURL:     "https://huggingface.co/Xenova/bge-small-en-v1.5/resolve/main/onnx/model.onnx",
		TokenizerURL: "https://huggingface.co/Xenova/bge-small-en-v1.5/resolve/main/tokenizer.json",
		ApproxMB:     127,
	},
	"bge-small-en-v1.5-int8": { //nolint:gosec // G101 false positive: the URLs below are public asset paths, not credentials
		// The same checkpoint with int8-quantized weights: a quarter of the
		// download and materially faster on CPU, at whatever accuracy the
		// quantization costs.
		//
		// It is a separate entry rather than a flag on the one above because
		// quantized weights produce different vectors, and Name is what makes
		// RepresentationID differ — so an index built with one is rebuilt
		// rather than silently compared against the other.
		Name:         "bge-small-en-v1.5-int8",
		Dim:          384,
		MaxSeqLen:    512,
		Pooling:      PoolCLS,
		QueryPrefix:  "Represent this sentence for searching relevant passages: ",
		ModelURL:     "https://huggingface.co/Xenova/bge-small-en-v1.5/resolve/main/onnx/model_quantized.onnx",
		TokenizerURL: "https://huggingface.co/Xenova/bge-small-en-v1.5/resolve/main/tokenizer.json",
		ApproxMB:     33,
	},
	"all-minilm-l6-v2": { //nolint:gosec // G101 false positive: the URLs below are public asset paths, not credentials
		// Kept past its retirement as the default because it is the baseline
		// every later checkpoint is judged against: without something to
		// compare to, "the new model is better" is an assertion. Its fields
		// reproduce the pre-0.1.3 representation exactly, so an index built
		// back then is still valid under this entry and does not rebuild.
		Name:         "all-MiniLM-L6-v2",
		Dim:          384,
		MaxSeqLen:    256,
		Pooling:      PoolMean,
		QueryPrefix:  "",
		ModelURL:     "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/onnx/model.onnx",
		TokenizerURL: "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/tokenizer.json",
		ApproxMB:     90,
	},
}

// Models returns every known checkpoint, ordered by name, for `semantic
// models`.
func Models() []*Model {
	out := make([]*Model, 0, len(registry))
	for _, m := range registry {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds a checkpoint by name, case-insensitively — the registry key is
// lowercased because `all-MiniLM-L6-v2` is not a name anyone types the same
// way twice.
func Lookup(name string) (*Model, bool) {
	m, ok := registry[strings.ToLower(strings.TrimSpace(name))]
	return m, ok
}

// ModelNames returns the known names in a form suitable for an error message.
func ModelNames() []string {
	out := make([]string, 0, len(registry))
	for _, m := range Models() {
		out = append(out, m.Name)
	}
	return out
}

var (
	// selMu guards current. Selection happens once at startup in the CLI, but
	// the benchmark switches models inside one process, so this is real.
	selMu   sync.Mutex
	current *Model
)

// Current returns the checkpoint in force, resolving $SEMANTIC_MODEL on first
// use and falling back to DefaultModel.
//
// An unrecognized $SEMANTIC_MODEL is ignored here rather than reported,
// because Current has nowhere to report it and silently embedding with a model
// nobody asked for is the worse of the two outcomes only if it is also the
// quiet one. The CLI calls Select up front, which does validate and does fail
// loudly, so the ignore path is reachable only by a library caller who set the
// variable without going through it.
func Current() *Model {
	selMu.Lock()
	defer selMu.Unlock()
	if current == nil {
		current = registry[strings.ToLower(DefaultModel)]
		if m, ok := Lookup(os.Getenv("SEMANTIC_MODEL")); ok {
			current = m
		}
	}
	return current
}

// Select switches the checkpoint by name and tears down any live ONNX session
// so the next embed builds one against the new weights. An empty name resolves
// $SEMANTIC_MODEL, then DefaultModel, which lets the CLI pass its flag value
// through unconditionally.
//
// Callers that switch mid-process — the benchmark is the only one — get a
// clean session per model. Callers that never call it get DefaultModel.
func Select(name string) error {
	if strings.TrimSpace(name) == "" {
		name = os.Getenv("SEMANTIC_MODEL")
	}
	if strings.TrimSpace(name) == "" {
		name = DefaultModel
	}
	m, ok := Lookup(name)
	if !ok {
		return fmt.Errorf("unknown embedding model %q — known models: %s",
			name, strings.Join(ModelNames(), ", "))
	}
	selMu.Lock()
	current = m
	selMu.Unlock()
	resetInferencer()
	return nil
}

// RepresentationID names the vector space the current model produces. It is
// the package-level spelling of Model.RepresentationID, kept because the index
// asks the package what it is embedding with, not which Model object is
// selected.
func RepresentationID() string {
	return Current().RepresentationID()
}
