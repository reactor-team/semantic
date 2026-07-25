// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package chunk

import (
	"strings"
	"testing"
)

const samplePython = `"""Module docstring."""

MAX_SIZE = 5
"""The largest allowed."""

_PRIVATE = 1
"""Hidden."""

UNDOCUMENTED = 3

# A leading comment.
class Server:
    """Serves things."""

    def start(self, ctx: int) -> bool:
        """Start it."""
        return True

@router.post("/sessions")
async def create(a, b=2) -> str:
    """Create a session."""
    return "x"
`

func TestPython(t *testing.T) {
	t.Parallel()
	got := Python(samplePython)

	cases := []struct {
		key     string
		variant string
		inText  []string
	}{
		{"py/module", VariantPackage, []string{"Module docstring."}},
		// PEP 258 attribute docstring: the bare string after an assignment.
		{"py/const/MAX_SIZE", VariantValue, []string{"MAX_SIZE = 5", "The largest allowed."}},
		{"py/class/Server", VariantClass, []string{"class Server", "Serves things."}},
		{"py/method/Server.start", VariantMethod, []string{"def start(self, ctx: int) -> bool", "Start it."}},
		// The decorator is kept: `@router.post("/sessions")` is the most
		// searchable line a route handler has.
		{"py/func/create", VariantFunc, []string{`@router.post("/sessions")`, "async def create(a, b=2) -> str", "Create a session."}},
	}
	for _, tc := range cases {
		c := find(got, tc.key)
		if c.Key == "" {
			t.Errorf("%s: not emitted (keys: %v)", tc.key, keysOf(got))
			continue
		}
		if c.Variant != tc.variant {
			t.Errorf("%s: variant = %q, want %q", tc.key, c.Variant, tc.variant)
		}
		for _, sub := range tc.inText {
			if !strings.Contains(c.Text, sub) {
				t.Errorf("%s: text missing %q\n---\n%s", tc.key, sub, c.Text)
			}
		}
	}

	// A method body is not embedded.
	if c := find(got, "py/method/Server.start"); strings.Contains(c.Text, "return True") {
		t.Errorf("method chunk should not carry the body: %q", c.Text)
	}
	// A leading underscore means private; the module's public surface is what
	// a search should return.
	if c := find(got, "py/const/_PRIVATE"); c.Key != "" {
		t.Error("a private constant should be skipped")
	}
	// Without a docstring an assignment is a bare name, and indexing every one
	// would bury the documented settings.
	if c := find(got, "py/const/UNDOCUMENTED"); c.Key != "" {
		t.Error("an undocumented constant should be skipped")
	}
}

// A docstring is the author's documentation and outranks a `#` comment above
// the def, which is usually an implementation note.
func TestPython_DocstringBeatsComment(t *testing.T) {
	t.Parallel()
	got := Python("# An implementation note.\ndef f():\n    \"\"\"The real documentation.\"\"\"\n    pass\n")
	c := find(got, "py/func/f")
	if c.Key == "" {
		t.Fatalf("no chunk for f (keys: %v)", keysOf(got))
	}
	if !strings.Contains(c.Text, "The real documentation.") {
		t.Errorf("docstring missing: %q", c.Text)
	}
	if strings.Contains(c.Text, "implementation note") {
		t.Errorf("the comment should not win over the docstring: %q", c.Text)
	}
}

// Where there is no docstring the comment is all the documentation there is,
// so it must still be indexed.
func TestPython_CommentUsedWhenNoDocstring(t *testing.T) {
	t.Parallel()
	got := Python("# The only documentation.\ndef f():\n    pass\n")
	if c := find(got, "py/func/f"); !strings.Contains(c.Text, "The only documentation.") {
		t.Errorf("comment not used as documentation: %q", c.Text)
	}
}

const sampleProto = `syntax = "proto3";
package reactor.v1;

// A Session is one run.
message Session {
  string id = 1;
}

// SessionService manages sessions.
service SessionService {
  // Create makes one.
  rpc Create(CreateRequest) returns (CreateResponse);
}

// Kind enumerates.
enum Kind { KIND_UNSPECIFIED = 0; }
`

func TestProtobuf(t *testing.T) {
	t.Parallel()
	got := Protobuf(sampleProto)

	for _, tc := range []struct {
		key     string
		variant string
		crumb   string
		inText  []string
	}{
		// A message renders whole: its fields are the schema.
		{"proto/message/Session", VariantMessage, "reactor.v1 > Session", []string{"string id = 1", "A Session is one run."}},
		{"proto/service/SessionService", VariantService, "reactor.v1 > SessionService", []string{"service SessionService", "manages sessions"}},
		{"proto/rpc/SessionService.Create", VariantRPC, "reactor.v1 > SessionService.Create", []string{"rpc Create(CreateRequest) returns (CreateResponse)", "Create makes one."}},
		{"proto/enum/Kind", VariantEnum, "reactor.v1 > Kind", []string{"KIND_UNSPECIFIED", "Kind enumerates."}},
	} {
		c := find(got, tc.key)
		if c.Key == "" {
			t.Errorf("%s: not emitted (keys: %v)", tc.key, keysOf(got))
			continue
		}
		if c.Variant != tc.variant {
			t.Errorf("%s: variant = %q, want %q", tc.key, c.Variant, tc.variant)
		}
		if c.Heading != tc.crumb {
			t.Errorf("%s: heading = %q, want %q", tc.key, c.Heading, tc.crumb)
		}
		for _, sub := range tc.inText {
			if !strings.Contains(c.Text, sub) {
				t.Errorf("%s: text missing %q\n---\n%s", tc.key, sub, c.Text)
			}
		}
	}

	// The service renders as its header. Its rpcs are chunks of their own, so
	// including the body would embed each one twice and drag a neighbouring
	// rpc's comment into the service's text.
	if c := find(got, "proto/service/SessionService"); strings.Contains(c.Text, "Create makes one.") {
		t.Errorf("service chunk should not carry its rpc bodies: %q", c.Text)
	}
}

func TestHCL(t *testing.T) {
	t.Parallel()
	got := HCL(`# The VPC for compute.
module "vpc" {
  source = "./modules/vpc"
}

resource "aws_s3_bucket" "logs" {
  bucket = "x"
  lifecycle_rule {
    enabled = true
  }
}
`)
	// A block is keyed by its type and labels joined — the name an engineer
	// uses for it in a plan, in state, and in conversation.
	for _, tc := range []struct{ key, inText string }{
		{"hcl/block/module.vpc", "The VPC for compute."},
		{"hcl/block/resource.aws_s3_bucket.logs", `bucket = "x"`},
		{"hcl/block/resource.aws_s3_bucket.logs.lifecycle_rule", "enabled = true"},
	} {
		c := find(got, tc.key)
		if c.Key == "" {
			t.Errorf("%s: not emitted (keys: %v)", tc.key, keysOf(got))
			continue
		}
		if !strings.Contains(c.Text, tc.inText) {
			t.Errorf("%s: text missing %q\n---\n%s", tc.key, tc.inText, c.Text)
		}
		if c.Variant != VariantBlock {
			t.Errorf("%s: variant = %q, want %q", tc.key, c.Variant, VariantBlock)
		}
	}
}

func TestBash(t *testing.T) {
	t.Parallel()
	got := Bash(`#!/usr/bin/env bash
# Rotate the certificates.

# Deploy the thing.
deploy() {
  echo hi
}

# The registry to push to.
REGISTRY=ghcr.io

TMP=/tmp/x
`)
	// The header describes the script, not the first declaration under it.
	// The two are different comments, separated by a blank line.
	h := find(got, "sh/header")
	if h.Key == "" {
		t.Fatalf("no header chunk (keys: %v)", keysOf(got))
	}
	if !strings.Contains(h.Text, "Rotate the certificates.") {
		t.Errorf("header text = %q, want the script's own comment", h.Text)
	}
	if strings.Contains(h.Text, "Deploy the thing.") {
		t.Errorf("the first function's comment was mislabelled as the header: %q", h.Text)
	}
	if strings.Contains(h.Text, "#!") {
		t.Errorf("the shebang names an interpreter and is not documentation: %q", h.Text)
	}

	if c := find(got, "sh/func/deploy"); !strings.Contains(c.Text, "Deploy the thing.") {
		t.Errorf("function documentation missing: %q", c.Text)
	}
	if c := find(got, "sh/var/REGISTRY"); !strings.Contains(c.Text, "The registry to push to.") {
		t.Errorf("documented variable missing its comment: %q", c.Text)
	}
	// A script is mostly assignments; indexing undocumented ones would bury
	// the useful chunks.
	if c := find(got, "sh/var/TMP"); c.Key != "" {
		t.Error("an undocumented variable should be skipped")
	}
}

func TestYAML(t *testing.T) {
	t.Parallel()
	got := YAML(`# The api-gateway deployment.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-gateway
spec:
  replicas: 2
---
kind: Service
metadata:
  name: sup-svc
spec:
  ports:
    - port: 80
`)
	// A document is identified by kind and metadata.name, because "the
	// api-gateway Deployment" is what someone searches for — not "the first
	// document in the file".
	for _, tc := range []struct {
		key, variant, inText string
	}{
		{"yaml/doc/Deployment/api-gateway", VariantDocument, "replicas: 2"},
		{"yaml/key/Deployment/api-gateway.spec", VariantSection, "replicas: 2"},
		{"yaml/doc/Service/sup-svc", VariantDocument, "port: 80"},
		{"yaml/key/Service/sup-svc.metadata", VariantSection, "name: sup-svc"},
	} {
		c := find(got, tc.key)
		if c.Key == "" {
			t.Errorf("%s: not emitted (keys: %v)", tc.key, keysOf(got))
			continue
		}
		if c.Variant != tc.variant {
			t.Errorf("%s: variant = %q, want %q", tc.key, c.Variant, tc.variant)
		}
		if !strings.Contains(c.Text, tc.inText) {
			t.Errorf("%s: text missing %q\n---\n%s", tc.key, tc.inText, c.Text)
		}
	}

	// A scalar key is already inside the document chunk and retrieves nothing
	// on its own.
	if c := find(got, "yaml/key/Deployment/api-gateway.kind"); c.Key != "" {
		t.Error("a scalar top-level key should not get its own chunk")
	}
	// The document separator is punctuation, not content.
	if c := find(got, "yaml/doc/Service/sup-svc"); strings.Contains(c.Text, "---") {
		t.Errorf("the --- separator leaked into the chunk: %q", c.Text)
	}
}

// A YAML file with no kind or name still has to produce distinct, stable keys.
func TestYAML_UnidentifiedDocumentsGetPositionalKeys(t *testing.T) {
	t.Parallel()
	got := YAML("a:\n  b: 1\n---\nc:\n  d: 2\n")
	if len(got) == 0 {
		t.Fatal("no chunks")
	}
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c.Key] {
			t.Errorf("duplicate key %q", c.Key)
		}
		seen[c.Key] = true
	}
}
