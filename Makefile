# attndb build targets.
#   make            native-free build (stub encoder) + tests target available
#   make onnx       build ./attndb with the real ONNX encoders (needs libs/ + models/)
#   make test       run the native-free test suite
#   make ingest     ingest corpus/ into Qdrant via the onnx binary
#   make search Q="..."   query the ingested Qdrant index

LIBS := $(CURDIR)/libs
ONNX := CGO_LDFLAGS="-L$(LIBS)" go build -tags onnx

.DEFAULT_GOAL := build
.PHONY: build onnx test vet fmt ingest search clean

build:
	go build ./...

onnx:
	$(ONNX) -o attndb ./cmd/attndb

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
