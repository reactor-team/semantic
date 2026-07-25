package embed

import "fmt"

// modelName is the checkpoint Get embeds with. It is part of the vector space's
// identity, not just a download detail: two indexes built with different
// checkpoints hold cosine-incomparable vectors even at the same dimension.
const modelName = "all-MiniLM-L6-v2"

// RepresentationID names the vector space this package produces. The index
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
// It is a function rather than a const so it stays derived from the constants
// it names. A hand-written string would be free to drift from them, which is
// the exact failure this guards against.
func RepresentationID() string {
	return fmt.Sprintf("%s+mean+l2+d%d+s%d", modelName, modelDim, maxSeqLen)
}
