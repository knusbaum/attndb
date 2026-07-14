# attndb build targets.
#   make            native-free build (stub encoder) + tests target available
#   make onnx       build ./attndb with the real ONNX encoders (needs libs/ + models/)
#   make test       run the native-free test suite
#   make ingest     ingest corpus/ into Qdrant via the onnx binary
#   make search Q="..."   query the ingested Qdrant index

LIBS := $(CURDIR)/libs
ONNX := CGO_LDFLAGS="-L$(LIBS)" go build -tags onnx

# Runtime ONNX Runtime shared library. The onnx encoder dlopen's this exact path
# (see internal/encode/onnx/colbert.go); exported so the ingest/search recipes
# pick it up without the caller having to set it.
export ATTNDB_ORT_LIB := $(LIBS)/libonnxruntime.1.27.0.dylib

# POST_ONNX runs after `make onnx` builds the binary — a seam for machine-local
# build steps that don't belong in this shared Makefile, chiefly codesigning on
# macOS (a stable signature keeps a Full Disk Access grant alive across rebuilds;
# Go's default ad-hoc signature changes every build and loses it). Empty here;
# set it in Makefile.local (git-ignored). See Makefile.local.example.
POST_ONNX ?=

# Machine-local overrides (git-ignored): POST_ONNX, personal run targets, etc.
# The leading '-' makes this a no-op when the file is absent.
-include Makefile.local

.DEFAULT_GOAL := build
.PHONY: build onnx test vet fmt ingest search clean

build:
	go build ./...

onnx:
	$(ONNX) -o attndb ./cmd/attndb
	$(POST_ONNX)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# Convenience targets for the play workflow (assume Qdrant up + corpus/ populated).
ingest: onnx
	./attndb ingest -store qdrant -encoder onnx -docs corpus/

search: onnx
	./attndb search -store qdrant -encoder onnx -docs corpus/ -k 5 "$(Q)"

clean:
	rm -f attndb
